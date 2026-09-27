# Smoke test — MQ Gateway v2 (`cmd/gateway`)

> **เอกสารนี้ให้ "คน" รันเอง ยังไม่เคยถูกรันจริง**
> ตอนทำ Task 13 เครื่อง dev มีแต่ Postgres และ RabbitMQ ของ **production** เท่านั้น
> ผู้ประสานงานจึงสั่งห้ามต่อของจริง ขั้นตอนทั้งหมดในไฟล์นี้จึง**ยังไม่มีผลรัน** —
> ทุกอย่างที่แต่ละขั้นคาดหวังมาจากการอ่านโค้ดและจาก unit test ไม่ใช่จากการรันกับของจริง
> อ่านหัวข้อ "ก่อนเริ่ม" ให้จบก่อนพิมพ์คำสั่งแรก

---

## ก่อนเริ่ม — 3 เรื่องที่พลาดแล้วกระทบ production

1. **`QUEUE_PREFIX` ห้ามว่างเด็ดขาด** เป็นสิ่งเดียวที่กันไม่ให้ service ใหม่ไปแย่งกิน queue
   ของระบบเก่าที่ใช้ broker ตัวเดียวกัน ชื่อ queue ที่ใช้จริงคือ `QUEUE_PREFIX + group_name`
   ถ้าเว้นว่างแล้วมี group ชื่อ `withdraw` service ใหม่จะไป consume queue `withdraw`
   **ตัวเดียวกับของเก่า** แล้วแย่งออเดอร์จริงมายิงทันที
   (`config.Load` บังคับให้ตั้งค่าอยู่แล้ว ถ้าว่างจะตายตอน start — แต่ห้ามตั้งเป็นค่าที่ชนของเก่า)

2. **`MIGRATE_ON_START` default เป็น `true`** แปลว่า start ครั้งแรกจะสร้าง/แก้ schema ใน DB ที่ชี้ไป
   **ให้ใช้ database แยกสำหรับ smoke test** (เช่น `dbname=mqsmoke`) ไม่ใช่ DB ของ production
   ถ้าจำเป็นต้องชี้ DB ที่มีของอยู่แล้ว ให้ตั้ง `MIGRATE_ON_START=false` แล้วรัน migration มือเอง

3. **`ALLOWED_IPS` ว่าง = ปิดทุกคน** (ทุก POST จะได้ 403 และจะไม่มีแถวใน `request_logs` เลย
   เพราะด่าน whitelist อยู่ก่อนทุกอย่างที่มี side effect) สำหรับ smoke test ให้ตั้ง `*`

---

## env ที่ต้องตั้ง

### บังคับ (ไม่ตั้ง = process ตายตอน start พร้อมบอกชื่อตัวแปรที่ขาด)

| ตัวแปร | ความหมาย | ตัวอย่าง |
|---|---|---|
| `RABBITMQ_URL` | DSN ของ broker | `amqp://user:pass@host:5672/` |
| `DATABASE_URL` | DSN ของ Postgres (lib/pq) | `host=... user=... password=... dbname=mqsmoke port=... sslmode=disable TimeZone=Asia/Bangkok` |
| `QUEUE_PREFIX` | prefix ของชื่อ queue — **ห้ามว่าง ห้ามชนของเก่า** | `v2.` |

### ไม่บังคับ (มี default)

| ตัวแปร | default | หมายเหตุ |
|---|---|---|
| `ALLOWED_IPS` | *(ว่าง = ปิดทุกคน)* | `*` = เปิดหมด หรือใส่ IP คั่น comma |
| `PORT` | `4000` | |
| `MIGRATE_ON_START` | `true` | `false`/`0`/`no` = ปิด — ดูข้อ 2 ข้างบน |
| `RECONCILE_INTERVAL` | `30s` | รอบที่ไปอ่าน `message_group` ใหม่ |
| `GRACEFUL_TIMEOUT` | `45s` | **งบรวมของ shutdown ทั้งก้อน** — ปิด HTTP server และ drain flow ใช้เวลาก้อนเดียวกันนี้ร่วมกัน เวลาปิดทั้งหมดจึงไม่เกินค่านี้ |
| `RPC_CHANNEL_POOL` | `4` | จำนวน channel สำหรับ RPC |
| `TRUSTED_PROXY_COUNT` | `1` | จำนวน proxy ที่ไว้ใจ นับ client IP ถอยจากขวาของ `X-Forwarded-For + RemoteAddr` |

> ถ้า smoke test ด้วย `curl` จาก localhost ตรง ๆ (ไม่ผ่าน proxy) และใช้ whitelist จริงแทน `*`
> ให้ตั้ง `TRUSTED_PROXY_COUNT=0` ด้วย ไม่งั้นการนับถอยจะไม่ตรงกับความเป็นจริง

```bash
export RABBITMQ_URL='amqp://USER:PASS@HOST:5672/'
export DATABASE_URL='host=HOST user=USER password=PASS dbname=mqsmoke port=PORT sslmode=disable TimeZone=Asia/Bangkok'
export QUEUE_PREFIX='v2.'          # ← ห้ามว่าง
export ALLOWED_IPS='*'
```

---

## ขั้นที่ 0 — ตรวจ "ก่อน" รัน (ต้องทำ ห้ามข้าม)

จดจำนวน consumer ของ queue ระบบเก่าไว้เทียบทีหลัง:

```bash
rabbitmqctl list_queues name consumers -p /
```

**จดตัวเลขของ 4 queue นี้ลงกระดาษ/ไฟล์:**

| queue | consumers ก่อนรัน |
|---|---|
| `withdraw` | |
| `deposit` | |
| `withdrawauto` | |
| `depositauto` | |

ถ้าคำสั่งนี้รันไม่ได้ (ไม่มีสิทธิ์ / broker อยู่ใน container) ให้ใช้ management UI หรือ
`docker exec <rabbitmq-container> rabbitmqctl list_queues name consumers -p /` แทน
**ถ้ายังไม่ได้ตัวเลขนี้ อย่าเริ่มขั้นที่ 1** เพราะจะไม่มีอะไรไว้ยืนยันว่าไม่ได้ไปแย่ง queue ของเก่า

---

## ขั้นที่ 1 — start service

```bash
go run ./cmd/gateway &
sleep 3
```

**คาดหวังใน log:** `✅ migration เรียบร้อย` (ถ้าเปิด migrate), `✅ ต่อ RabbitMQ แล้ว (pool 4 ช่อง, prefix "v2.")`,
`🌐 ฟังอยู่ที่ :4000`

ถ้า config ขาด จะเห็น `❌ config ไม่ถูกต้อง: ...` พร้อมชื่อตัวแปรที่ขาด แล้ว process ตายทันที
(ตรวจแล้วว่าพฤติกรรมนี้ทำงานจริง — รัน binary ด้วย env เปล่า ดูรายงาน Task 13)

---

## ขั้นที่ 2 — ยังไม่มี group ในตาราง

```bash
curl -s localhost:4000/healthz
curl -s localhost:4000/readyz
curl -s -o /dev/null -w '%{http_code}\n' -X POST localhost:4000/withdraw
```

**คาดหวัง**
- `/healthz` → `ok` (200) ตอบได้ตลอดแม้ flow ยังไม่ขึ้น
- `/readyz` → `{"ready":true,"flows":{}}` (ยังไม่มี flow = ไม่มีตัวไหนล้ม)
- `POST /withdraw` → **404** (ไม่มี group ชื่อนี้ใน registry)

---

## ขั้นที่ 3 — เพิ่ม group แล้วรอ reconcile

```bash
psql "$DATABASE_URL" -c "
  WITH g AS (INSERT INTO message_group (group_name) VALUES ('smoke') RETURNING id)
  INSERT INTO message_group_url (message_group_id, url)
  SELECT id, 'https://httpbin.org/post' FROM g;"

sleep 35     # ต้องไม่เกิน RECONCILE_INTERVAL (default 30s)
```

`message_group` มี default ครบทุกคอลัมน์ (`worker_count=50`, `upstream_timeout_ms=30000`,
`rpc_timeout_ms=60000`, `ref_field='customer_order_id'`) จึงใส่แค่ `group_name` ได้

> **ไม่ต้อง curl อะไรเพื่อ start consumer เลย** — นี่คือ pain point ข้อ 4 ของระบบเก่าที่ถูกแก้
> reconcile loop เห็น group ใหม่แล้วสร้าง queue + consumer ให้เอง

---

## ขั้นที่ 4 — ยิงจริง

```bash
curl -s localhost:4000/readyz
curl -s -D- -X POST localhost:4000/smoke -d '{"customer_order_id":"SMOKE-1"}'
```

**คาดหวัง**
- `/readyz` → `{"ready":true,"flows":{"smoke":"running"}}`
- `POST /smoke` → **200** พร้อม header `X-Trace-Id: <uuid>` และ body เป็น response ของ
  `httpbin.org/post` แบบ passthrough

> ⚠️ **HTTP status เป็น passthrough ของ upstream (spec §7.7) ต่างจากระบบเก่าที่ตอบ 200 เสมอ**
> ถ้า upstream ตอบ 400 caller จะได้ 400 ไม่ใช่ 200 — ข้อนี้ต้องยืนยันกับ caller จริง
> ในระยะที่ 2 ของ cutover (spec §8.5) ก่อนเปลี่ยน route ของจริงมาที่ service นี้

---

## ขั้นที่ 5 — อ่าน log ใน DB

```bash
psql "$DATABASE_URL" -c "
  SELECT trace_id, group_name, status, http_status, attempt_count, total_ms, business_ref
  FROM request_logs ORDER BY created_at DESC LIMIT 5;"

psql "$DATABASE_URL" -c "
  SELECT seq, url, http_status, outcome, duration_ms
  FROM attempt_logs ORDER BY id DESC LIMIT 5;"
```

**คาดหวัง**
- `request_logs` มีแถว `status='success'`, `http_status=200`, `business_ref='SMOKE-1'`
  (ดึงจากฟิลด์ตาม `ref_field` ของ group)
- `attempt_logs` มีแถว `seq=1`, `outcome='success'` ผูกกับ `trace_id` เดียวกัน
- `trace_id` ตรงกับ `X-Trace-Id` ที่ได้จาก header ในขั้นที่ 4

---

## ขั้นที่ 6 — ตรวจ "หลัง" รัน ว่าไม่ได้ไปแตะระบบเก่า (ต้องทำ ห้ามข้าม)

```bash
rabbitmqctl list_queues name consumers -p /
```

**เกณฑ์ผ่าน — ต้องครบทั้ง 2 ข้อ**

1. จำนวน consumer ของ `withdraw`, `deposit`, `withdrawauto`, `depositauto`
   **เท่ากับตัวเลขที่จดไว้ในขั้นที่ 0 ทุกตัว ไม่เปลี่ยนแม้แต่ queue เดียว**
2. **เห็น queue ใหม่แยกออกมา** ชื่อขึ้นต้นด้วย `QUEUE_PREFIX` (เช่น `v2.smoke`) และมี consumer

```bash
# ดูเฉพาะของใหม่
rabbitmqctl list_queues name consumers -p / | grep '^v2\.'
```

**ถ้าข้อ 1 ไม่ผ่าน** (consumer ของ queue เก่าเปลี่ยนไป) แปลว่า service ใหม่ไปแย่ง queue ของ
production เข้าแล้ว — **ปิด service ทันที** แล้วไปทำ rollback ข้างล่าง ตรวจ `QUEUE_PREFIX`
และชื่อ group ใน `message_group` ว่าประกอบกันแล้วชนชื่อ queue เก่าหรือไม่

---

## Rollback

```bash
# 1. ปิด service
kill %1                     # ถ้ารันด้วย go run ... &
#   หรือถ้า deploy เป็น Easypanel service: stop service นั้น

# 2. ยืนยันว่า consumer ของเราหายไปแล้ว (queue เก่าต้องไม่กระทบ)
rabbitmqctl list_queues name consumers -p /

# 3. ลบ queue ที่ขึ้นต้นด้วย prefix ทิ้ง — ลบเฉพาะของเรา
rabbitmqctl list_queues name -p / | grep '^v2\.' | while read -r q; do
  rabbitmqctl delete_queue "$q" -p /
done
```

**ห้ามลบ queue ที่ไม่ได้ขึ้นต้นด้วย prefix** — `withdraw`, `deposit`, `withdrawauto`,
`depositauto` เป็นของระบบเก่าที่ยังรับออเดอร์จริงอยู่

ถ้าจะล้าง DB ที่ใช้ smoke test ด้วย (เฉพาะ database แยกที่สร้างมาเพื่อ test เท่านั้น):

```bash
psql "$DATABASE_URL" -c "DELETE FROM message_group WHERE group_name = 'smoke';"
# message_group_url และ request_logs/attempt_logs ตามไปด้วยผ่าน ON DELETE CASCADE
# (request_logs ไม่ได้ FK กับ message_group จึงเหลือแถวไว้ ลบมือถ้าต้องการ)
```

---

## หลังผ่าน smoke test แล้ว

ตาม spec §8.5 ระยะที่ 1 และ dispatch ของ Task 13:

1. deploy เป็น **Easypanel service ใหม่** โดย `QUEUE_PREFIX` ต้องไม่ว่าง และ
   `message_group_url.url` ชี้ **sandbox** ก่อน ไม่ใช่ปลายทางจริง
2. ตั้ง health check ของ Easypanel เป็น `/readyz`
3. ตั้ง **stop timeout ของ Easypanel ให้มากกว่า `GRACEFUL_TIMEOUT` พอสมควร**
   ที่ค่า default 45s ให้ตั้ง **≥ 60s** (เหลือ headroom ~15s)
   เวลาปิดทั้งหมดถูกจำกัดด้วย `GRACEFUL_TIMEOUT` ก้อนเดียว: `srv.Shutdown` ใช้ไปเท่าไหร่
   `drainAll` ได้เวลาที่เหลือ ไม่ใช่ได้งบใหม่เต็มก้อน ดังนั้น worst case = `GRACEFUL_TIMEOUT`
   ไม่ใช่สองเท่า
   ถ้าตั้ง stop timeout น้อยกว่างบนี้ จะโดน SIGKILL กลาง drain แบบที่ระบบเก่าโดนทุกครั้ง
   (ของเก่าต้องใช้ 30s แต่ docker ให้ 10s) — ข้อความที่ยังไม่ ack จะถูก requeue ทั้งหมด
4. ยังไม่เปลี่ยน route ของ caller จริงจนกว่าจะยืนยันเรื่อง HTTP status passthrough ในขั้นที่ 4
