# เช็คลิสต์ขึ้น production — MQ Gateway v2

ปรับปรุง 2026-09-28 · ใช้คู่กับ `smoke-test-gateway.md` (ขั้นตอนทดสอบ+rollback)
และ `v1-vs-v2.md` (สิ่งที่เปลี่ยนสำหรับ caller)

---

## ⚠️ ข้อที่พลาดแล้วเจ็บที่สุด: DB ใหม่ไม่มี group เลย

`migrate` สร้างแต่ตาราง **ไม่ใส่ข้อมูลสักแถว** (`0001_init.sql` มี `INSERT` 0 คำสั่ง)
ซ้อมบน DB เปล่าจริงแล้ว 2026-09-28 ได้ผลนี้:

```
attempt_logs        0 แถว
message_group       0 แถว   ← ไม่มี group
message_group_url   0 แถว
request_logs        0 แถว
schema_migrations   1 แถว
```

**ผลคือ service สตาร์ทสำเร็จ `/readyz` ตอบ `{"flows":{},"ready":true}` แต่ทุก request ได้ 404**
เขียวทั้งที่ตายสนิท — เป็นกับดักเดียวกับ v1 ที่ `/online` ตอบ 200 ขณะ consumer ตายหมด
ซึ่งเป็นเหตุผลข้อแรกที่ทำ refactor นี้

**ทางแก้: รัน `./scripts/seed-groups.sh --apply` หลัง deploy ครั้งแรกเสมอ** (ข้อ 6 ด้านล่าง)

---

## 1. ก่อนถึงวัน cutover

| # | เรื่อง | ทำไม |
|---|---|---|
| 1.1 | **ยืนยันกับทุกทีมที่เรียกเข้ามาว่าโค้ดตัดสินใจจาก HTTP status หรือ `code` ใน body** | v1 ตอบ 200 เสมอ v2 ส่ง status ของ upstream กลับตรง ๆ ใครเช็ค `if status == 200` จะพังทันที ถ้ารับไม่ได้แก้ไม่กี่บรรทัดใน `internal/httpapi/handler.go` |
| 1.2 | **ไล่เก็บ IP ของทุกทีมที่ยิง `deposit` / `depositauto` / `withdrawauto`** | v1 เช็ค IP แค่เส้น `withdraw` เส้นเดียว อีก 3 เส้นถูก comment ทิ้ง (`homeController.go:195, 226, 257`) รายการ `WISHLIST_IP` เดิม **ไม่ครบแน่นอน** |
| 1.3 | ทำความสะอาดรายการ IP | ของเดิม 92 รายการ: ซ้ำ 4, รูปแบบผิด 1 (`43.228.126.241.152.42.192.121` ทำให้ 2 IP ใช้ไม่ได้จริงโดยไม่มีใครรู้), private 8 |
| 1.4 | เช็คว่ามี service ภายในเรียก gateway ตรง ๆ โดยไม่ผ่าน proxy ไหม | ถ้ามี IP ที่เห็นจะเป็น private (`192.168.x.x`) ต้องคง IP พวกนั้นไว้ |
| 1.5 | **นับจำนวน proxy ที่คั่นบน prod** | `TRUSTED_PROXY_COUNT` ของ prod อาจไม่เท่า dev ตั้งผิด = allowlist เชื่อถือไม่ได้ทั้งหมด วิธีตรวจอยู่ข้อ 8.2 |
| 1.6 | ยืนยัน Postgres ของ prod เป็น **เวอร์ชัน 13 ขึ้นไป** | migration ใช้ `gen_random_uuid()` ซึ่งเป็น built-in ตั้งแต่ 13 ถ้าต่ำกว่านั้นต้องมี `pgcrypto` (โค้ดพยายามสร้างให้แบบ best-effort แล้ว) — dev ใช้ 17.11 |
| 1.7 | ยืนยัน DB user มีสิทธิ์ `CREATE TABLE` | migration รันตอน start ถ้าไม่มีสิทธิ์ service จะตายพร้อม `❌ migration ล้มเหลว` |
| 1.8 | **ปิดพอร์ต Postgres จากภายนอก** | server ไม่รองรับ TLS (`sslmode=require` → `server does not support SSL`) ทุกการเข้าไปดูข้อมูลจากข้างนอกส่งรหัสผ่านและ `request_body` ที่มีเลขบัญชีลูกค้าเป็น plaintext — ให้เหลือ localhost แล้วใช้ SSH tunnel |

---

## 2. env ที่ต้องตั้ง

```bash
# บังคับ 4 ตัว — ขาดตัวใดตัวหนึ่ง service ไม่สตาร์ท
RABBITMQ_URL=amqp://...
DATABASE_URL=host=... user=... password=... dbname=... port=... sslmode=disable TimeZone=Asia/Bangkok
QUEUE_PREFIX=v2.
ALLOWED_IPS=1.2.3.4,5.6.7.8          # รายการจริง ไม่ใช่ * และ **ห้ามใส่เครื่องหมายคำพูด**

# ตามสภาพแวดล้อม
PORT=80                              # ให้ตรงกับพอร์ตที่ proxy ชี้มา
TRUSTED_PROXY_COUNT=1                # ตามข้อ 1.5
TZ=Asia/Bangkok                      # ไม่ตั้ง log จะเป็น UTC ต่างจากนาฬิกา 7 ชั่วโมง

# ที่เหลือมี default ใช้ได้เลย
# RECONCILE_INTERVAL=30s  GRACEFUL_TIMEOUT=45s  RPC_CHANNEL_POOL=4  MIGRATE_ON_START=true
```

**`QUEUE_PREFIX` ห้ามว่างตราบใดที่ v1 ยังรันอยู่บน broker เดียวกัน** ชื่อ group ของเราคือ
`deposit`/`withdraw`/… เหมือน v1 เป๊ะ ถ้าไม่มี prefix consumer สองฝั่งจะ round-robin
**แบ่งออเดอร์จริงกันคนละครึ่งโดยไม่มี error ให้เห็นเลย** โค้ดปฏิเสธค่าว่างตั้งแต่ start

**`ALLOWED_IPS` ห้ามใส่เครื่องหมายคำพูด** โค้ดไม่ได้ถอดให้ `"1.2.3.4"` จะกลายเป็น IP ที่ไม่มีทาง match
ยิ่งหลายค่าจะพังแบบครึ่ง ๆ (เครื่องหมายไปติดตัวแรกกับตัวสุดท้าย ตัวกลางรอด) ซึ่งหาสาเหตุยากกว่าพังทั้งหมด
`DATABASE_URL` ที่มีเว้นวรรคข้างในและไม่ใส่คำพูดก็ทำงานได้ — แบบไม่ใส่คือแบบที่พิสูจน์แล้ว

---

## 3–7. ลำดับการ deploy

| # | ขั้นตอน | ตรวจว่าสำเร็จ |
|---|---|---|
| 3 | เตรียม DB ของ prod (สร้าง database เปล่า) | ต่อได้ |
| 4 | deploy service ด้วย `Dockerfile.gateway` พร้อม env ข้อ 2 | container รันอยู่ |
| 5 | migration รันเองตอน start | log มี `✅ migration เรียบร้อย` และ `schema_migrations` มี 1 แถว |
| 6 | **`./scripts/seed-groups.sh --apply`** ← ข้อที่ห้ามลืม | สคริปต์พิมพ์ตาราง 4 group แต่ละตัวมี 4 url |
| 7 | รอไม่เกิน `RECONCILE_INTERVAL` | `/readyz` ขึ้นครบ 4 เส้นเป็น `running` |

`seed-groups.sh` อ่าน url จาก `*_URL_GROUP` ใน `CLAUDE.local.md` (หรือไฟล์ที่ส่งผ่าน `ENVFILE=`)
จึงไม่มี url ฝังอยู่ใน git ตรวจค่าให้ก่อนสร้าง SQL ด้วยว่า

- `upstream_timeout × จำนวน url ≤ rpc_timeout` (7000×4 = 28000 ≤ 30000)
- `rpc_timeout + 10s ≤ GRACEFUL_TIMEOUT` (30000+10000 = 40000 ≤ 45000)

รันซ้ำได้ไม่เสียหาย ทุกคำสั่งเป็น `ON CONFLICT` — ทดสอบแล้วว่า md5 ของสถานะ DB เท่าเดิมเป๊ะ
ดูอย่างเดียวไม่รันจริงใช้ `./scripts/seed-groups.sh` เฉย ๆ

---

## 8. ตรวจก่อนเปลี่ยน route

### 8.1 ชุดทดสอบเต็ม

```bash
GATEWAY=https://<prod-url> ./scripts/full-test.sh
```

ต้องผ่านครบ 31 ข้อ **รันจากเครื่องที่อยู่ใน `ALLOWED_IPS`**
หมวด B จะสร้างออเดอร์จริง 4 รายการ — ใช้ MID ทดสอบถ้าไม่ต้องการให้กระทบยอดจริง

### 8.2 ยืนยัน `TRUSTED_PROXY_COUNT` ด้วยของจริง

ยิงหนึ่ง request แล้วดูว่าระบบบันทึก IP อะไร ต้องตรงกับ IP สาธารณะของเครื่องที่ยิง:

```sql
SELECT client_ip, created_at FROM request_logs ORDER BY created_at DESC LIMIT 1;
```

ถ้าได้ IP ของ proxy แสดงว่าตั้งน้อยไป ถ้าได้ค่าที่ caller ใส่มาเองแสดงว่าตั้งมากไป

### 8.3 ยืนยันว่า allowlist หลอกไม่ได้

```bash
curl -X POST https://<prod-url>/deposit -H 'X-Forwarded-For: <IP ที่อยู่ในรายการ>' -d '{}'
```

ยิงจากเครื่องที่ **ไม่อยู่** ในรายการ ต้องได้ `403` ถ้าได้อย่างอื่นแปลว่า `TRUSTED_PROXY_COUNT` ผิด
(ทดสอบบน dev แล้ว 8 วิธี ไม่ผ่านสักวิธี)

### 8.4 ยืนยันว่าไม่ไปแตะ queue ของ v1

```bash
rabbitmqctl list_queues name consumers | grep -E '^(deposit|withdraw)'
```

queue ของ v1 ต้องมี consumer เท่าเดิม ส่วนของเราต้องขึ้นต้นด้วย `v2.` ทั้งหมด

---

## 9. เปลี่ยน route แล้วเฝ้า

เปลี่ยน domain ให้ชี้มาที่ service ใหม่ **v1 ยังรันอยู่ ไม่ต้องปิด** แล้วเฝ้า 3 เรื่อง:

```sql
-- 1. มีอะไรค้าง pending ไหม
SELECT count(*) FROM request_logs WHERE status='pending' AND created_at < now() - interval '1 min';

-- 2. มี 403 โผล่ไหม (= IP ที่ยังไม่ได้ใส่ในรายการ) — ดูจาก log หา 🚫 ปฏิเสธ IP

-- 3. เคสที่ต้องกระทบยอดด้วยมือ
SELECT r.trace_id, r.business_ref, a.url, a.error_message
FROM request_logs r JOIN attempt_logs a USING (trace_id)
WHERE a.outcome='fatal' AND a.http_status IS NULL AND r.created_at > now() - interval '1 hour';
```

**rollback**: ชี้ domain กลับไปที่ v1 — มันรันอยู่ตลอดและไม่ถูกแตะเลย ใช้เวลาเท่าที่ DNS/proxy ใช้
ขั้นตอนเก็บกวาดแบบละเอียดอยู่ใน `smoke-test-gateway.md`

---

## 10. เรื่องที่ยังไม่ได้ตัดสิน

**`/readyz` ควรตอบ not-ready เมื่อมี 0 flow ไหม** ตอนนี้ registry ว่าง = `ready:true`
ซึ่งทำให้ prod ที่ยังไม่ได้ seed ขึ้นมาเขียวทั้งที่ทุก request 404

- **ข้อดีถ้าเปลี่ยน** ปัญหาดังทันที ไม่มีทางเงียบ ตรงกับปรัชญาที่ว่า readiness ต้องบอกความจริง
- **ข้อเสีย** restart ไม่ได้ช่วยให้มี group ขึ้นมา จะกลายเป็น pod ที่ไม่มีวันพร้อมจนกว่าคนจะ seed
  และบล็อกกรณีที่ตั้งใจรันโดยยังไม่มี group (เช่น ทยอยเปิดทีละเส้น)

ทางสายกลางที่ไม่ต้องแลกอะไร: log เตือนซ้ำทุกรอบ reconcile ตราบใดที่ยังมี 0 group
แบบเดียวกับ `warnIfTimeoutsExceedGrace` ที่มีอยู่แล้ว — **ยังไม่ได้ทำ รอตัดสินใจ**

งานค้างอื่นอยู่ใน `mq-gateway-v2-followups.md`
