# ยิงโหลดใส่ MQ Gateway v2

ตอบสองคำถามพร้อมกัน: **รับไหวไหม** และ **ตอนรับไม่ไหวมันพังแบบไหน** — ข้อหลังสำคัญกว่า
เพราะระบบจ่ายเงินที่พังแบบ "สร้างออเดอร์ซ้ำ" แย่กว่าพังแบบ "ปฏิเสธไปเลย"

ระดับโหลดทั้งหมดอิง `docs/traffic-analysis.md` (ข้อมูลจริง 7 วัน)

| | รายการ/นาที | rps |
|---|---:|---:|
| เฉลี่ย | 10.6 | 0.18 |
| p99 | 35 | 0.58 |
| **พีคจริงที่เคยเจอ** | **103** | **1.7** ← TARGET |

> พีค 103/นาที เกิดเมื่อ 2026-09-19 20:52 และตัวเลขนี้**นับเฉพาะรายการที่ไปถึง backend สำเร็จ**
> รายการที่ตายตอน TLS handshake ไม่ถูกนับเลยสักแถว ของจริงจึงสูงกว่านี้ — เป็นเหตุผลที่
> `stress` ไต่ไปถึง 100 เท่าของพีค ไม่ใช่แค่ 2-3 เท่า

---

## ⚠️ ตั้ง upstream ให้ถูกก่อนยิง — อ่านก่อน

gateway จะ **ยิงต่อไปยัง url ใน `message_group_url` จริง ๆ** ดังนั้นปลายทางคือสิ่งที่ต้องเลือกให้ดี

| ปลายทาง | ผลข้างเคียง | ใช้กับ |
|---|---|---|
| `http://127.0.0.1:80/healthz` | **ไม่มีเลย** — gateway ยิงหาตัวเอง | ทุก scenario ✅ |
| `https://api.goquickpay.com/api/v1/test/rabbitmq` | เขียน `system_logs` บน **prod** 1 แถวต่อ request | smoke สั้น ๆ เท่านั้น |
| url ของ deposit/withdraw จริง | **สร้างออเดอร์จริง เงินออกจริง** | ❌ ห้ามเด็ดขาด |

`stress` ที่ 200 rps × 3 นาที = ~36,000 request ถ้าชี้ไป prod echo จะเขียน `system_logs`
บน production 36,000 แถว **อย่าทำ**

### ตั้งกลุ่มสำหรับยิงโหลด

```sql
WITH g AS (
  INSERT INTO message_group (group_name, ref_field, worker_count, upstream_timeout_ms, rpc_timeout_ms)
  VALUES ('loadtest', 'customer_order_id', 50, 10000, 60000)
  ON CONFLICT (group_name) DO UPDATE SET updated_at = now() RETURNING id)
INSERT INTO message_group_url (message_group_id, url)
SELECT id, 'http://127.0.0.1:80/healthz' FROM g
ON CONFLICT (message_group_id, url) DO NOTHING;
```

รอไม่เกิน 30 วินาที (`RECONCILE_INTERVAL`) แล้วเช็คว่าขึ้นแล้ว

```bash
curl -s "$BASE/readyz"     # ต้องเห็น "loadtest":"running"
```

---

## รันยังไง

```bash
brew install k6     # ยังไม่มีในเครื่อง

export BASE='https://deposit-service-mq-v2.ebwved.easypanel.host'
export GROUP='loadtest'
export TOKEN='<ถ้า upstream ต้องใช้>'

SCENARIO=smoke  k6 run loadtest/k6/gateway.js   # 2 rps 30 วิ — รันก่อนทุกครั้ง
SCENARIO=load   k6 run loadtest/k6/gateway.js   # 2 rps 10 นาที
SCENARIO=spike  k6 run loadtest/k6/gateway.js   # กระโดด 12 เท่าใน 10 วิ
SCENARIO=stress k6 run loadtest/k6/gateway.js   # ไต่ถึง 200 rps หาเพดาน
SCENARIO=soak   k6 run loadtest/k6/gateway.js   # 1 rps 2 ชั่วโมง
```

**ต้องรัน reconcile ทุกครั้งหลังยิงจบ** — k6 บอกได้แค่ว่า caller เห็นอะไร

```bash
psql "$DATABASE_URL" -v ref_prefix="'LT-%'" -f loadtest/reconcile.sql
```

| scenario | ยิงอะไร | ตอบคำถามอะไร |
|---|---|---|
| `smoke` | 2 rps 30 วิ | เส้นทางยังถูกไหม |
| `load` | 2 rps 10 นาที | ที่พีคจริง นิ่งไหม |
| `spike` | กระโดด 12 เท่าใน 10 วิ | จำลองพีค 20:52 — กลับมาเองไหม |
| `stress` | ไต่ถึง 100 เท่าของพีค | เพดานอยู่กี่ rps และพังแบบไหน |
| `soak` | ครึ่งพีค 2 ชั่วโมง | memory รั่ว / คิวค้างสะสมไหม |

---

## อ่านผลยังไง

**k6 บอก**: latency ที่ caller เห็น, จำนวนที่ไม่ใช่ 200, throughput จริง

**`reconcile.sql` บอกสิ่งที่สำคัญกว่า**:

- **ข้อ 2 — เคสที่ต้องกระทบยอด** ควรเป็น **0** ถ้าไม่ใช่ แปลว่ามี request ที่ยิง upstream ไปแล้ว
  แต่ caller ไม่ได้รับคำตอบ — ออเดอร์อาจเกิดจริง ต้องไล่ทีละรายการ
- **ข้อ 3 — `expired`** ยิ่งเยอะยิ่งแปลว่าคิวล้น worker ตามไม่ทัน **แต่ปลอดภัย** เพราะ
  ยิง upstream 0 ครั้ง นี่คือพฤติกรรมที่ถูกต้องเมื่อรับไม่ไหว ไม่ใช่ความล้มเหลว
- **ข้อ 5 — `worker_ที่พอ`** คือคำตอบของคำถามที่ `traffic-analysis.md` §5 เปิดค้างไว้
  ว่า "ต้องวัด p95 ก่อนถึงจะตั้ง worker ได้"
- **ข้อ 7 — `pending` ค้าง** ต้องเป็น 0 ถ้ามีค้างแปลว่ามี request ที่ไม่มีใครปิดงานให้

### เกณฑ์ผ่าน

| | ต้องได้ |
|---|---|
| ที่พีคจริง (2 rps) | 200 ทุกรายการ, p95 < 2 วินาที |
| ตอน spike | อาจมี `expired` ได้ แต่ **ห้ามมีเคสข้อ 2** |
| ตอน stress เกินเพดาน | ต้องปฏิเสธ (`expired`/504) ไม่ใช่สร้างออเดอร์ซ้ำ |
| หลังยิงจบทุกครั้ง | `pending` = 0, `/readyz` กลับมา ready |

---

## เก็บกวาด

```sql
DELETE FROM message_group WHERE group_name = 'loadtest';
-- log เก็บไว้ได้ ใช้เทียบครั้งหน้า ถ้าจะลบ:
-- DELETE FROM request_logs WHERE business_ref LIKE 'LT-%';
```
