#!/usr/bin/env bash
# ยิงรายการฝาก/ถอนจริงผ่าน MQ Gateway v2 แล้วตามรอยด้วย trace_id
#
# ความลับทั้งหมด (token, mid, DSN) อ่านจาก CLAUDE.local.md ซึ่ง .gitignore กันไว้แล้ว
# ไฟล์นี้จึง commit ได้โดยไม่มีอะไรรั่ว
#
#   ./scripts/live-test.sh deposit|depositauto|withdraw|withdrawauto|all [amount]
#   ./scripts/live-test.sh trace <trace_id>     # ดูผลของรายการที่ยิงไปแล้ว
#   ./scripts/live-test.sh recent [n]           # ดู n รายการล่าสุด
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOCAL="$ROOT/CLAUDE.local.md"
[[ -f "$LOCAL" ]] || { echo "ไม่พบ $LOCAL"; exit 1; }

GATEWAY="${GATEWAY:-https://deposit-service-mq-v2.ebwved.easypanel.host}"
MID="$(grep -m1 '^mid *:' "$LOCAL" | sed 's/^mid *: *//; s/ .*//')"
TOKEN="$(grep -m1 '^api token *:' "$LOCAL" | sed 's/^api token *: *//; s/[[:space:]]*$//')"
CALLBACK="$(grep -m1 '^callback url *:' "$LOCAL" | sed 's/^callback url *: *//; s/[[:space:]]*$//')"
DSN="$(grep -m1 '^DATABASE_URL=' "$LOCAL" | sed 's/^DATABASE_URL=//')"

# แปลง DSN แบบ key=value ของ gorm เป็นอาร์กิวเมนต์ psql
pg() {
  PGPASSWORD="$(sed -n 's/.*password=\([^ ]*\).*/\1/p' <<<"$DSN")" \
  psql -h "$(sed -n 's/.*host=\([^ ]*\).*/\1/p' <<<"$DSN")" \
       -p "$(sed -n 's/.*port=\([^ ]*\).*/\1/p' <<<"$DSN")" \
       -U "$(sed -n 's/.* user=\([^ ]*\).*/\1/p' <<<"$DSN")" \
       -d "$(sed -n 's/.*dbname=\([^ ]*\).*/\1/p' <<<"$DSN")" "$@"
}

order_id() { echo "MQV2-$(date +%Y%m%d-%H%M%S)-$RANDOM"; }

payload() { # $1=flow  $2=order_id  $3=amount
  # เส้น auto ไม่ต้องส่ง mid — ฝั่ง quickpay ผูก mid ไว้กับ user แล้วเลือกให้เอง
  case "$1" in
    deposit) cat <<JSON
{"account_name":"test agent auto","account_number":"1234567890","amount":$3,
 "bank_code":"004","callback_url":"$CALLBACK","ref1":"$2",
 "customer_order_id":"$2","mid":"$MID","qr_type":"promptpay"}
JSON
    ;;
    depositauto) cat <<JSON
{"account_name":"test agent auto","account_number":"1234567890","amount":$3,
 "bank_code":"004","callback_url":"$CALLBACK","ref1":"$2",
 "customer_order_id":"$2","qr_type":"promptpay"}
JSON
    ;;
    withdraw) cat <<JSON
{"customer_order_id":"$2","mid":"$MID","account_number":"1234567890",
 "bank_code":"KBANK","bank_name":"KASIKORNBANK","account_name":"test agent auto",
 "amount":$3,"cost":0,"withdraw_type":"normal","settlement":"auto",
 "non_funded_type":"","callback_url":"$CALLBACK","remark":"mq gateway v2 live test"}
JSON
    ;;
    withdrawauto) cat <<JSON
{"customer_order_id":"$2","account_number":"1234567890",
 "bank_code":"KBANK","bank_name":"KASIKORNBANK","account_name":"test agent auto",
 "amount":$3,"cost":0,"withdraw_type":"normal","settlement":"auto",
 "non_funded_type":"","callback_url":"$CALLBACK","remark":"mq gateway v2 live test"}
JSON
    ;;
  esac
}

fire() { # $1=flow  $2=amount
  local flow="$1" amount="$2" oid body out status trace
  oid="$(order_id)"
  body="$(payload "$flow" "$oid" "$amount")"
  echo "────────────────────────────────────────────────────────"
  echo "▶  $flow  order=$oid  amount=$amount"
  out="$(curl -sS -o /tmp/mqv2.body -D /tmp/mqv2.head -w '%{http_code} %{time_total}' \
        --max-time 60 -X POST "$GATEWAY/$flow" \
        -H 'Content-Type: application/json' -H "Authorization: Bearer $TOKEN" \
        --data-binary "$body")" || true
  status="${out%% *}"; trace="$(sed -n 's/^[Xx]-[Tt]race-[Ii]d: *//p' /tmp/mqv2.head | tr -d '\r')"
  echo "   HTTP $status  ใช้เวลา ${out##* }s  trace=$trace"
  echo "   ↳ $(head -c 600 /tmp/mqv2.body)"
  [[ -n "$trace" ]] && echo "$trace" >> /tmp/mqv2.traces
}

case "${1:-all}" in
  deposit|depositauto|withdraw|withdrawauto) fire "$1" "${2:-100}" ;;
  all) # เพิ่มยอดทีละบาท เพราะถอนซ้ำ account+amount เดิมในเวลาใกล้กันโดน Duplicate withdraw request
       n=0; for f in deposit depositauto withdraw withdrawauto; do fire "$f" "$(( ${2:-100} + n ))"; n=$((n+1)); done ;;
  trace)
    pg -x -c "SELECT r.trace_id, r.group_name, r.business_ref, r.status, r.http_status,
                     r.created_at, r.finished_at,
                     (SELECT count(*) FROM attempt_logs a WHERE a.trace_id=r.trace_id) AS attempts
              FROM request_logs r WHERE r.trace_id='$2'"
    pg -c "SELECT seq, url, http_status, duration_ms, outcome, left(coalesce(error_message,''),60) AS err
           FROM attempt_logs WHERE trace_id='$2' ORDER BY seq" ;;
  recent)
    pg -c "SELECT left(trace_id::text,8) AS trace, group_name, business_ref, status, http_status,
                  round(extract(epoch FROM (finished_at-created_at))*1000)::int AS ms, created_at
           FROM request_logs ORDER BY created_at DESC LIMIT ${2:-10}" ;;
  *) echo "ใช้: $0 deposit|depositauto|withdraw|withdrawauto|all [amount] | trace <id> | recent [n]"; exit 1 ;;
esac
