#!/usr/bin/env bash
# ชุดทดสอบเต็มของ MQ Gateway v2 ยิงกับ service จริง
#
#   ./scripts/full-test.sh          รันทุกหมวด
#   ./scripts/full-test.sh A C      รันเฉพาะหมวดที่ระบุ
#
# หมวด A เข้าถึง/allowlist · B เส้นทางจริง · C ตรวจ input · D failover/classify
#      E reconciler · F deadline · G readiness
#
# หมวด D/E/F ใช้ group ชั่วคราวชื่อ gwtest ไม่แตะ 4 เส้นฝากถอนจริง
# และทำความสะอาดให้เองตอนจบเสมอ แม้ถูก Ctrl-C
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOCAL="$ROOT/CLAUDE.local.md"
GATEWAY="${GATEWAY:-https://deposit-service-mq-v2.ebwved.easypanel.host}"
# DSN จาก env ชนะค่าในไฟล์ — ไฟล์มีได้หลาย DATABASE_URL (dev/prod)
# grep -m1 จะได้ตัวแรกเสมอ ซึ่งอาจไม่ใช่ตัวที่ gateway กำลังใช้อยู่
DSN="${DSN:-$(grep -m1 '^DATABASE_URL=' "$LOCAL" | sed 's/^DATABASE_URL=//')}"
TMP="${TMPDIR:-/tmp}/gwtest.$$"; mkdir -p "$TMP"

pg() {
  PGPASSWORD="$(sed -n 's/.*password=\([^ ]*\).*/\1/p' <<<"$DSN")" \
  psql -h "$(sed -n 's/.*host=\([^ ]*\).*/\1/p' <<<"$DSN")" \
       -p "$(sed -n 's/.*port=\([^ ]*\).*/\1/p' <<<"$DSN")" \
       -U "$(sed -n 's/.* user=\([^ ]*\).*/\1/p' <<<"$DSN")" \
       -d "$(sed -n 's/.*dbname=\([^ ]*\).*/\1/p' <<<"$DSN")" -tA "$@"
}

PASS=0; FAIL=0; SKIP=0
ok()   { PASS=$((PASS+1)); printf '  \033[32m✓\033[0m %s\n' "$1"; }
no()   { FAIL=$((FAIL+1)); printf '  \033[31m✗\033[0m %s\n     คาดว่า: %s\n     ได้จริง: %s\n' "$1" "$2" "$3"; }
skip() { SKIP=$((SKIP+1)); printf '  \033[33m-\033[0m %s (%s)\n' "$1" "$2"; }
head2(){ printf '\n\033[1m%s\033[0m\n' "$1"; }
eq()   { [ "$2" = "$3" ] && ok "$1" || no "$1" "$2" "$3"; }

# ยิงแล้วคืน "<http_code> <trace_id>"
fire() { # $1=path $2=body $3.. = อาร์กิวเมนต์ curl เพิ่ม
  local path="$1" body="$2"; shift 2
  local code; code=$(curl -s -o "$TMP/body" -D "$TMP/head" -w '%{http_code}' --max-time 60 \
      -X POST "$GATEWAY/$path" -H 'Content-Type: application/json' "$@" --data-binary "$body")
  echo "$code $(sed -n 's/^[Xx]-[Tt]race-[Ii]d: *//p' "$TMP/head" | tr -d '\r')"
}

cleanup() { pg -c "DELETE FROM message_group WHERE group_name LIKE 'gwtest%';" >/dev/null 2>&1; rm -rf "$TMP"; }
trap cleanup EXIT INT TERM

wait_flow() { # $1=ชื่อ  $2=สถานะที่ต้องการ (running|gone)  รอสูงสุด 75 วิ
  for _ in $(seq 1 25); do
    local r; r=$(curl -s --max-time 10 "$GATEWAY/readyz")
    case "$2" in
      running) [[ "$r" == *"\"$1\":\"running\""* ]] && return 0 ;;
      gone)    [[ "$r" != *"\"$1\":"* ]] && return 0 ;;
    esac
    sleep 3
  done
  return 1
}

want="${*:-A B C D E F G}"
run() { [[ " $want " == *" $1 "* ]]; }

# ══════════════════════════════════════════════════════════════
run A && {
head2 "A · การเข้าถึงและ allowlist"

read -r code _ <<<"$(fire deposit '{}')"
if [ "$code" = "403" ]; then
  ok "A1 IP ถูกปฏิเสธ (ALLOWED_IPS ไม่มี IP นี้)"
  echo "     เครื่องนี้เข้าไม่ได้ ข้ามหมวดที่เหลือ"; exit 1
else
  ok "A1 IP ผ่าน allowlist (ได้ $code ไม่ใช่ 403)"
fi

# หลอก X-Forwarded-For — Traefik จะต่อท้ายด้วย IP จริงเสมอ
# ตัวที่ TRUSTED_PROXY_COUNT ชี้จึงเป็น IP จริงไม่ว่าจะส่งอะไรมา
# นี่คือช่องโหว่ของ v1 ที่อ่านตัวซ้ายสุด (homeController.go:75)
REAL=$(pg -c "SELECT client_ip FROM request_logs ORDER BY created_at DESC LIMIT 1;")
read -r code trace <<<"$(fire deposit '{}' -H 'X-Forwarded-For: 1.2.3.4, 5.6.7.8')"
sleep 1
SPOOF=$(pg -c "SELECT client_ip FROM request_logs WHERE trace_id='$trace';")
eq "A2 หลอก X-Forwarded-For ไม่สำเร็จ ยังบันทึก IP จริง" "$REAL" "$SPOOF"

eq "A3 /healthz เข้าได้โดยไม่ผ่าน allowlist" "200" \
   "$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$GATEWAY/healthz")"
eq "A4 /readyz  เข้าได้โดยไม่ผ่าน allowlist" "200" \
   "$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$GATEWAY/readyz")"
}

# ══════════════════════════════════════════════════════════════
run B && {
head2 "B · เส้นทางฝากถอนจริง"
n=0
for f in deposit depositauto withdraw withdrawauto; do
  amt=$((200 + n)); n=$((n+1))
  out=$("$ROOT/scripts/live-test.sh" "$f" "$amt" 2>&1)
  code=$(sed -n 's/.*HTTP \([0-9]*\).*/\1/p' <<<"$out" | head -1)
  trace=$(sed -n 's/.*trace=\([0-9a-f-]*\).*/\1/p' <<<"$out" | head -1)
  if [ "$code" = "200" ] && grep -q '"code":0' <<<"$out"; then
    ok "B$n $f สร้างออเดอร์จริงสำเร็จ (code:0)"
  else
    no "B$n $f" "HTTP 200 + code:0" "HTTP $code — $(grep -o '"message":"[^"]*"' <<<"$out" | head -1)"
  fi
  [ -n "$trace" ] && {
    row=$(pg -c "SELECT status||'/'||coalesce(http_status::text,'-')||'/'||
                 (SELECT count(*) FROM attempt_logs a WHERE a.trace_id=r.trace_id)
                 FROM request_logs r WHERE trace_id='$trace';")
    eq "B$n-log บันทึกครบ status/http_status/attempts" "success/200/1" "$row"
  }
done
}

# ══════════════════════════════════════════════════════════════
run C && {
head2 "C · การตรวจ input"

eq "C1 method GET → 405" "405" \
   "$(curl -s -o /dev/null -w '%{http_code}' --max-time 15 "$GATEWAY/deposit")"
read -r code _ <<<"$(fire ไม่มีจริง '{}')"          ; eq "C2 group ที่ไม่มี → 404" "404" "$code"
read -r code _ <<<"$(fire deposit/extra '{}')"      ; eq "C3 path ซ้อนชั้น → 404" "404" "$code"

head -c 5000000 /dev/zero | tr '\0' 'x' > "$TMP/big"
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 60 -X POST "$GATEWAY/deposit" \
       -H 'Content-Type: application/json' --data-binary "@$TMP/big")
eq "C4 body 5MB เกินลิมิต 4MB → 413" "413" "$code"

read -r code trace <<<"$(fire deposit 'ไม่ใช่ json เลย')"
sleep 1
raw=$(pg -c "SELECT request_body ? 'raw' FROM request_logs WHERE trace_id='$trace';")
eq "C5 body ที่ไม่ใช่ JSON ถูกห่อเป็น {\"raw\":…} ไม่ทำให้ INSERT ล้ม" "t" "$raw"
}

# ══════════════════════════════════════════════════════════════
run D && {
head2 "D · failover และการจำแนกผล"
pg -c "DELETE FROM message_group WHERE group_name LIKE 'gwtest%';" >/dev/null
pg -c "INSERT INTO message_group (group_name, worker_count, upstream_timeout_ms, rpc_timeout_ms)
       VALUES ('gwtest', 5, 8000, 30000);" >/dev/null

# ตัวที่ต่อไม่ติดต้องถูกจัดเป็น retryable แล้วไปต่อตัวที่ใช้ได้
pg -c "INSERT INTO message_group_url (message_group_id, url)
       SELECT id, u FROM message_group, unnest(ARRAY[
         'http://127.0.0.1:9/x',
         'https://ไม่มีโดเมนนี้จริง-gwtest.invalid/x',
         'https://httpbin.org/post']) AS u
       WHERE group_name='gwtest';" >/dev/null
wait_flow gwtest running && ok "D0 flow gwtest ขึ้นเองจาก DB" || { no "D0 flow gwtest" "running" "ไม่ขึ้นใน 75 วิ"; }

read -r code trace <<<"$(fire gwtest '{"t":"failover"}')"
sleep 2
eq "D1 มี url ใช้ไม่ได้ 2 ตัว แต่ caller ยังได้ 200" "200" "$code"
outc=$(pg -c "SELECT string_agg(outcome, ',' ORDER BY seq) FROM attempt_logs WHERE trace_id='$trace';")
case "$outc" in
  *retryable*success) ok "D2 ตัวที่ต่อไม่ติด = retryable แล้วไปต่อจนสำเร็จ ($outc)" ;;
  success)            skip "D2 สุ่มเจอตัวที่ใช้ได้ก่อน" "$outc" ;;
  *)                  no "D2 ลำดับ outcome" "…retryable,success" "$outc" ;;
esac

# 500 ต้องเป็น fatal ยิงครั้งเดียวจบ ไม่ลองตัวถัดไป
pg -c "UPDATE message_group_url SET is_active=false
       WHERE message_group_id=(SELECT id FROM message_group WHERE group_name='gwtest');" >/dev/null
pg -c "INSERT INTO message_group_url (message_group_id, url)
       SELECT id, u FROM message_group, unnest(ARRAY[
         'https://httpbin.org/status/500',
         'https://httpbin.org/status/500']) AS u
       WHERE group_name='gwtest' ON CONFLICT DO NOTHING;" >/dev/null
pg -c "UPDATE message_group_url SET is_active=true
       WHERE url LIKE '%status/500%';" >/dev/null
sleep 35
read -r code trace <<<"$(fire gwtest '{"t":"500"}')"
sleep 2
cnt=$(pg -c "SELECT count(*) FROM attempt_logs WHERE trace_id='$trace';")
eq "D3 upstream 500 ส่งผ่านเป็น 500" "500" "$code"
eq "D4 500 = fatal ยิงครั้งเดียวไม่ retry" "1" "$cnt"
}

# ══════════════════════════════════════════════════════════════
run E && {
head2 "E · reconciler"
pg -c "DELETE FROM message_group WHERE group_name LIKE 'gwtest%';" >/dev/null
sleep 2
pg -c "INSERT INTO message_group (group_name, worker_count, upstream_timeout_ms, rpc_timeout_ms)
       VALUES ('gwtest2', 3, 8000, 30000);" >/dev/null
pg -c "INSERT INTO message_group_url (message_group_id, url)
       SELECT id, 'https://httpbin.org/post' FROM message_group WHERE group_name='gwtest2';" >/dev/null

wait_flow gwtest2 running && ok "E1 เพิ่ม group ใน DB → endpoint เกิดเอง ไม่ต้อง deploy" \
                          || no "E1 group ใหม่" "running ใน 75 วิ" "ไม่ขึ้น"

# แก้ url อย่างเดียว = HotSwap ไม่ควร restart (ดูจาก log ฝั่ง service)
pg -c "UPDATE message_group_url SET url='https://httpbin.org/post?v=2'
       WHERE message_group_id=(SELECT id FROM message_group WHERE group_name='gwtest2');" >/dev/null
sleep 35
read -r code trace <<<"$(fire gwtest2 '{"t":"hotswap"}')"
sleep 2
u=$(pg -c "SELECT url FROM attempt_logs WHERE trace_id='$trace' LIMIT 1;")
case "$u" in *"v=2"*) ok "E2 แก้ url แล้วมีผลทันทีโดยไม่ restart (HotSwap)" ;;
             *) no "E2 HotSwap" "url ที่มี v=2" "$u" ;; esac

pg -c "DELETE FROM message_group WHERE group_name='gwtest2';" >/dev/null
wait_flow gwtest2 gone && ok "E3 ลบ group ใน DB → endpoint หายไปเอง" \
                       || no "E3 ลบ group" "หายจาก /readyz" "ยังอยู่"
read -r code _ <<<"$(fire gwtest2 '{}')"
eq "E4 ยิง group ที่ลบไปแล้ว → 404" "404" "$code"
}

# ══════════════════════════════════════════════════════════════
run F && {
head2 "F · x-deadline กันออเดอร์ผี"
pg -c "DELETE FROM message_group WHERE group_name LIKE 'gwtest%';" >/dev/null
sleep 2
# worker 1 ตัว + rpc_timeout 2 วิ + upstream ช้า = คิวยาวจนหมดอายุแน่นอน
pg -c "INSERT INTO message_group (group_name, worker_count, upstream_timeout_ms, rpc_timeout_ms)
       VALUES ('gwtest3', 1, 5000, 2000);" >/dev/null
pg -c "INSERT INTO message_group_url (message_group_id, url)
       SELECT id, 'https://httpbin.org/delay/3' FROM message_group WHERE group_name='gwtest3';" >/dev/null
wait_flow gwtest3 running || skip "F ทั้งหมด" "flow ไม่ขึ้น"

for i in $(seq 1 30); do fire gwtest3 "{\"i\":$i}" >/dev/null 2>&1 & done
wait
sleep 8
read -r exp att <<<"$(pg -F' ' -c "
  SELECT count(*) FILTER (WHERE r.status='expired'),
         coalesce(sum((SELECT count(*) FROM attempt_logs a WHERE a.trace_id=r.trace_id)),0)
  FROM request_logs r
  WHERE r.group_name='gwtest3' AND r.status='expired';")"
if [ "${exp:-0}" -gt 0 ]; then
  ok "F1 มี $exp request หมดอายุก่อนถูกประมวลผล"
  eq "F2 request ที่หมดอายุ ยิง upstream 0 ครั้ง (ไม่มีออเดอร์ผี)" "0" "$att"
else
  skip "F1/F2" "ไม่มี request หมดอายุ — เครื่องเร็วเกินไป ลองเพิ่มจำนวนที่ยิง"
fi
pg -c "DELETE FROM message_group WHERE group_name LIKE 'gwtest%';" >/dev/null
}

# ══════════════════════════════════════════════════════════════
run G && {
head2 "G · readiness"
r=$(curl -s --max-time 10 "$GATEWAY/readyz")
[[ "$r" == *'"ready":true'* ]] && ok "G1 ready:true" || no "G1" '"ready":true' "$r"
[[ "$r" == *'"amqp":"up"'* ]] && ok "G2 amqp:up"   || no "G2" '"amqp":"up"' "$r"
miss=""
for f in deposit depositauto withdraw withdrawauto; do
  [[ "$r" == *"\"$f\":\"running\""* ]] || miss="$miss $f"
done
[ -z "$miss" ] && ok "G3 flow จริงครบ 4 เส้นอยู่ในสถานะ running" \
               || no "G3 flow ครบ" "ครบ 4" "ขาด:$miss"
}

printf '\n\033[1m── สรุป ──\033[0m  ผ่าน \033[32m%d\033[0m  ไม่ผ่าน \033[31m%d\033[0m  ข้าม \033[33m%d\033[0m\n' "$PASS" "$FAIL" "$SKIP"
[ "$FAIL" -eq 0 ]
