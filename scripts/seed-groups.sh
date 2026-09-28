#!/usr/bin/env bash
# สร้าง 4 group ฝากถอนลง DB ที่เพิ่ง migrate ใหม่
#
# migration สร้างแต่ตาราง ไม่ใส่ข้อมูล — DB ใหม่จึงไม่มี group เลย
# service จะสตาร์ทได้ปกติ /readyz ตอบ ready:true แต่ทุก request ได้ 404
# สคริปต์นี้คือขั้นตอนที่ปิดช่องนั้น
#
#   ./scripts/seed-groups.sh                      แสดง SQL อย่างเดียว (ค่าเริ่มต้น)
#   ./scripts/seed-groups.sh --apply              รันจริงกับ DSN ใน CLAUDE.local.md
#   ENVFILE=prod.env ./scripts/seed-groups.sh --apply --dsn "host=... dbname=..."
#
# รันซ้ำได้ไม่เสียหาย — ใช้ ON CONFLICT ทั้งหมด url เดิมไม่ถูกแตะ
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENVFILE="${ENVFILE:-$ROOT/CLAUDE.local.md}"
APPLY=false; DSN=""
while [ $# -gt 0 ]; do
  case "$1" in
    --apply) APPLY=true ;;
    --dsn)   DSN="$2"; shift ;;
    *) echo "ไม่รู้จักตัวเลือก $1"; exit 1 ;;
  esac; shift
done
[ -f "$ENVFILE" ] || { echo "ไม่พบ $ENVFILE"; exit 1; }
[ -n "$DSN" ] || DSN="$(grep -m1 '^DATABASE_URL=' "$ENVFILE" | sed 's/^DATABASE_URL=//')"

# ค่าเหล่านี้ต้องสอดคล้องกันสองข้อพร้อมกัน
#   1) UPSTREAM × จำนวน url ≤ RPC     caller ต้องไม่ได้ 504 ก่อนระบบได้ลอง url ครบ
#   2) RPC + 10s ≤ GRACEFUL_TIMEOUT   deploy ต้องไม่ตัดงานที่ค้างอยู่
# 4 url → 7000×4 = 28000 ≤ 30000 และ 30000 + 10000 = 40000 ≤ 45000  ผ่านทั้งคู่
WORKERS="${WORKERS:-50}"
UPSTREAM="${UPSTREAM:-7000}"
RPC="${RPC:-30000}"
REF="${REF:-customer_order_id}"
GRACE_MS="${GRACE_MS:-45000}"

urls_of() { # อ่าน *_URL_GROUP แล้วคืนทีละบรรทัด
  grep -m1 "^$1" "$ENVFILE" | sed 's/^[^=]*= *//; s/^"//; s/"$//' | tr ',' '\n' | sed 's/^ *//; s/ *$//' | grep .
}

# group → ชื่อตัวแปรใน env file
# GROUPS เป็นตัวแปรพิเศษของ bash (array ของ group id ระบบ) ตั้งทับไม่ได้ ใช้ชื่ออื่น
FLOWMAP="withdraw:WITHDRAW_URL_GROUP withdrawauto:WITHDRAW_AUTO_URL_GROUP deposit:DEPOSIT_URL_GROUP depositauto:DEPOSIT_AUTO_URL_GROUP"

# ── ตรวจก่อนสร้าง SQL ─────────────────────────────────────────
fail=0
for pair in $FLOWMAP; do
  g="${pair%%:*}"; v="${pair##*:}"
  n=$(urls_of "$v" | wc -l | tr -d ' ')
  [ "$n" -gt 0 ] || { echo "❌ $v ไม่มี url"; fail=1; continue; }
  bad=$(urls_of "$v" | grep -vcE '^https?://' || true)
  [ "$bad" -eq 0 ] || { echo "❌ $v มี url ที่ไม่ขึ้นต้นด้วย http(s):// จำนวน $bad"; fail=1; }
  dup=$(( n - $(urls_of "$v" | sort -u | wc -l | tr -d ' ') ))
  [ "$dup" -eq 0 ] || echo "⚠️  $v มี url ซ้ำ $dup ตัว (ON CONFLICT จะกรองให้)"
  if [ $((UPSTREAM * n)) -gt "$RPC" ]; then
    echo "❌ $g: UPSTREAM $UPSTREAM × $n url = $((UPSTREAM*n))ms เกิน RPC ${RPC}ms"
    echo "   → ลด UPSTREAM เหลือ $((RPC / n))ms หรือเพิ่ม RPC"
    fail=1
  fi
  printf '   %-14s %d url  worst case %dms\n' "$g" "$n" $((UPSTREAM*n))
done
if [ $((RPC + 10000)) -gt "$GRACE_MS" ]; then
  echo "❌ RPC ${RPC}ms + 10s เกิน GRACEFUL_TIMEOUT ${GRACE_MS}ms — deploy จะตัดงานที่ค้าง"; fail=1
fi
[ "$fail" -eq 0 ] || { echo; echo "ไม่สร้าง SQL เพราะค่าขัดกัน แก้ก่อน"; exit 1; }
echo "   ✓ ค่า timeout สอดคล้องกันทั้งหมด"; echo

# ── สร้าง SQL ─────────────────────────────────────────────────
SQL=$(mktemp)
{
  echo "BEGIN;"
  for pair in $FLOWMAP; do
    g="${pair%%:*}"; v="${pair##*:}"
    cat <<SQL
INSERT INTO message_group (group_name, ref_field, worker_count, upstream_timeout_ms, rpc_timeout_ms)
VALUES ('$g', '$REF', $WORKERS, $UPSTREAM, $RPC)
ON CONFLICT (group_name) DO UPDATE SET
  ref_field=EXCLUDED.ref_field, worker_count=EXCLUDED.worker_count,
  upstream_timeout_ms=EXCLUDED.upstream_timeout_ms, rpc_timeout_ms=EXCLUDED.rpc_timeout_ms,
  updated_at=now();
SQL
    while read -r u; do
      cat <<SQL
INSERT INTO message_group_url (message_group_id, url)
SELECT id, '$u' FROM message_group WHERE group_name='$g'
ON CONFLICT (message_group_id, url) DO NOTHING;
SQL
    done < <(urls_of "$v")
  done
  echo "COMMIT;"
} > "$SQL"

if [ "$APPLY" != true ]; then
  cat "$SQL"; rm -f "$SQL"
  echo; echo "── นี่คือโหมดแสดงอย่างเดียว ใส่ --apply เพื่อรันจริง ──"
  exit 0
fi

PGPASSWORD="$(sed -n 's/.*password=\([^ ]*\).*/\1/p' <<<"$DSN")" \
psql -h "$(sed -n 's/.*host=\([^ ]*\).*/\1/p' <<<"$DSN")" \
     -p "$(sed -n 's/.*port=\([^ ]*\).*/\1/p' <<<"$DSN")" \
     -U "$(sed -n 's/.* user=\([^ ]*\).*/\1/p' <<<"$DSN")" \
     -d "$(sed -n 's/.*dbname=\([^ ]*\).*/\1/p' <<<"$DSN")" \
     -v ON_ERROR_STOP=1 -q -f "$SQL"
rm -f "$SQL"

echo "seed เรียบร้อย — สถานะใน DB ตอนนี้:"
PGPASSWORD="$(sed -n 's/.*password=\([^ ]*\).*/\1/p' <<<"$DSN")" \
psql -h "$(sed -n 's/.*host=\([^ ]*\).*/\1/p' <<<"$DSN")" \
     -p "$(sed -n 's/.*port=\([^ ]*\).*/\1/p' <<<"$DSN")" \
     -U "$(sed -n 's/.* user=\([^ ]*\).*/\1/p' <<<"$DSN")" \
     -d "$(sed -n 's/.*dbname=\([^ ]*\).*/\1/p' <<<"$DSN")" \
     -c "SELECT g.group_name, g.worker_count AS worker, g.upstream_timeout_ms AS upstream,
                g.rpc_timeout_ms AS rpc, count(u.*) AS urls
         FROM message_group g LEFT JOIN message_group_url u
              ON u.message_group_id=g.id AND u.is_active
         GROUP BY g.id, g.group_name, g.worker_count, g.upstream_timeout_ms, g.rpc_timeout_ms
         ORDER BY 1;"
echo "รอไม่เกิน RECONCILE_INTERVAL แล้วเช็ค /readyz ว่าขึ้นครบ 4 เส้น"
