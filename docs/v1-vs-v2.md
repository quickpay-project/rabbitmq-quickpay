# เทียบระบบเก่า (v1) กับ MQ Gateway v2

ปรับปรุงล่าสุด 2026-09-28 — v2 ทดสอบบน dev ครบทุกกลไกและยิงรายการฝากถอนจริงผ่านแล้ว

v1 คือโค้ดใน `controllers/` `rabbitMQ/` `route/` `main.go` ที่ยังรันบน production อยู่
v2 คือ `cmd/gateway` + `internal/*` ยังไม่ตัดเข้า production

---

## 1. env — อันไหนอยู่ อันไหนออก อันไหนเปลี่ยน

**จาก 13 ตัวใน `.env` เดิม เหลือ 4 ตัวที่ต้องตั้ง**

### อยู่เหมือนเดิม

| ตัวแปร | หมายเหตุ |
|---|---|
| `RABBITMQ_URL` | เหมือนเดิมทุกอย่าง |
| `DATABASE_URL` | ชื่อเดิม แต่**น้ำหนักเปลี่ยน** — v1 ใช้แค่เขียน log (ซึ่งตารางว่างเปล่ามาตลอด ดู `traffic-analysis.md` §6) ส่วน v2 เก็บ group/url/worker/timeout ไว้ที่นี่ทั้งหมด ต่อไม่ได้ = ไม่มี flow ไหนเกิดเลย |

### เปลี่ยนชื่อ

| v1 | v2 | ต้องระวัง |
|---|---|---|
| `WISHLIST_IP` | **`ALLOWED_IPS`** | ค่าเหมือนเดิม (รายชื่อคั่นด้วย `,`) แต่ถ้าก๊อป env เก่ามาทั้งชุดโดยไม่เปลี่ยนชื่อ `ALLOWED_IPS` จะว่าง → **403 ทุก request** เพราะว่าง = ปฏิเสธทุกคน (fail-closed ตั้งใจ) ใส่ `*` ถ้าจะเปิดหมด |

### ย้ายเข้า DB — เอาออกจาก env ได้เลย

| v1 | ไปอยู่ที่ | ได้อะไร |
|---|---|---|
| `DEPOSIT_URL_GROUP` | `message_group_url.url` ของ group `deposit` | แก้ url / ปิดตัวที่ล่ม **ไม่ต้อง deploy** รอไม่เกิน `RECONCILE_INTERVAL` |
| `DEPOSIT_AUTO_URL_GROUP` | ↳ group `depositauto` | ↑ |
| `WITHDRAW_URL_GROUP` | ↳ group `withdraw` | ↑ |
| `WITHDRAW_AUTO_URL_GROUP` | ↳ group `withdrawauto` | ↑ |
| `DEPOSIT_LIMIT` | `message_group.worker_count` | v1 ตั้งผิดรูปแบบ = `log.Fatalf` ฆ่าทั้ง container |
| `WITHDRAW_LIMIT` | ↳ ของ group ถอน | ↑ |

v1 ใช้สองตัวนี้ไม่เท่ากันด้วยซ้ำ — ฝั่งฝากเอาไปตั้งทั้ง prefetch `ch.Qos(workerCount)`
(`rabbitMQ/consdeposit.go:228`) และปั่น goroutine เท่าจำนวนนั้น (`:241`)
ส่วนฝั่งถอนปั่น goroutine อย่างเดียว ไม่ตั้ง `Qos` เลย (`rabbitMQ/conswithdraw.go:212`)
คิวถอนจึงดึงข้อความมากองไว้ไม่จำกัด ใน v2 `worker_count` แปลว่า worker pool เท่ากันทุก group

ค่าที่ v1 ไม่เคยมีให้ตั้ง แต่ v2 ตั้งได้ต่อ group ใน DB:
`upstream_timeout_ms` `rpc_timeout_ms` `ref_field` `is_active`

### ลบทิ้ง — ไม่มีโค้ดบรรทัดไหนอ่านตั้งแต่ v1 แล้ว

| ตัวแปร | สถานะ |
|---|---|
| `BALANCE_LIMIT` | ไม่มีใครอ่าน |
| `CONFIRMORDER_LIMIT` | ไม่มีใครอ่าน |
| `DEPOSIT_GROUP_STATUS` | ไม่มีใครอ่าน — ตั้งใจจะใช้เปิด/ปิด url รายตัวแต่ทำไม่เสร็จ v2 ทำจริงด้วย `message_group_url.is_active` |
| `WITHDRAW_GROUP_STATUS` | ↑ |

### ของใหม่ — บังคับ

| ตัวแปร | ค่า | ทำไม |
|---|---|---|
| `QUEUE_PREFIX` | `v2.` | **ตัวกันชนกับ v1** group ชื่อ `deposit` → queue จริง `v2.deposit` ถ้าปล่อยว่างแล้วรันคู่ของเก่าบน broker เดียวกัน consumer สองฝั่งจะ round-robin **แบ่งออเดอร์จริงกันคนละครึ่งโดยไม่มี error ให้เห็นเลย** `config.Load` จึงปฏิเสธค่าว่างตั้งแต่ start |

### ของใหม่ — ไม่บังคับ มี default

| ตัวแปร | default | ตั้งเมื่อไหร่ |
|---|---|---|
| `PORT` | `4000` | dev ตั้ง `80` ตามที่ Easypanel ชี้มา |
| `TRUSTED_PROXY_COUNT` | `1` | จำนวน proxy ที่คั่น — หลัง Traefik ตัวเดียว = 1, มี Cloudflare ด้วย = 2 **ตั้งผิดแล้ว allowlist เชื่อถือไม่ได้** |
| `RECONCILE_INTERVAL` | `30s` | รอบที่ไปอ่าน DB มาปรับ flow ให้ตรง |
| `GRACEFUL_TIMEOUT` | `45s` | ต้อง `≥ rpc_timeout + 10s` เสมอ ไม่งั้น deploy ตัดงานที่ค้างอยู่ |
| `RPC_CHANNEL_POOL` | `4` | AMQP channel ฝั่งรับ HTTP |
| `MIGRATE_ON_START` | `true` | ปล่อยไว้ มี `pg_advisory_lock` กันหลาย instance ชนกันแล้ว |

### ชุดที่ต้องตั้งจริง

```bash
RABBITMQ_URL=…          # เดิม
DATABASE_URL=…          # เดิม
QUEUE_PREFIX=v2.        # ใหม่ ห้ามลืม
ALLOWED_IPS=…           # เดิมชื่อ WISHLIST_IP
```

**สองจุดที่พลาดแล้วเจ็บ** — ลืมเปลี่ยน `WISHLIST_IP` → `ALLOWED_IPS` ได้ 403 ทุกเส้นทันที
และลืม `QUEUE_PREFIX` service ไม่สตาร์ท (ซึ่งดีกว่าสตาร์ทแล้วไปแย่งออเดอร์เงียบ ๆ)

---

## 2. สิ่งที่ลูกค้าผู้เรียกเห็น

### เหมือนเดิม ไม่ต้องแก้โค้ดฝั่ง caller

| | |
|---|---|
| Method / Path | `POST /deposit` `/depositauto` `/withdraw` `/withdrawauto` |
| Body | ฟิลด์ชุดเดิมทั้งหมด v2 **ไม่แตะ body เลย** ส่งต่อ byte ต่อ byte อ่านแค่ `ref_field` ไปเก็บ log |
| Header auth | `Authorization: Bearer <token>` ส่งต่อขึ้น upstream เหมือนเดิม |
| Response body | ของ upstream ดิบ ๆ `Content-Type: application/json` รูปแบบ `{"code":0,…}` เหมือนเดิม |

### ที่เปลี่ยน — 5 จุด

| # | เรื่อง | v1 | v2 | ผลต่อ caller |
|---|---|---|---|---|
| 1 | **HTTP status** | `200` เสมอ (`w.Write` ไม่เคยเรียก `WriteHeader`) | ส่ง status ของ upstream กลับตรง ๆ | ยิง deposit ด้วย `bank_code` ผิดรูปแบบ v2 ตอบ **400** v1 ตอบ **200** พร้อม body เดียวกัน — ใครเขียน `if status == 200` แล้วค่อยอ่าน `code` จะพัง |
| 2 | **IP allowlist** | บังคับแค่ `withdraw` เส้นเดียว อีก 3 เส้นถูก comment ทิ้ง (`controllers/homeController.go:195, 226, 257`) | บังคับครบ 4 เส้น | **จุดที่จะเงียบ ๆ แล้วพัง** ใครยิง deposit ได้ทุกวันนี้โดยไม่เคยอยู่ใน whitelist จะโดน 403 ทันทีที่ตัด |
| 3 | **เวลารอสูงสุด** | รอ reply 90s แต่ worker ตั้ง `http.Client{Timeout: 300s}` — ขัดกันเอง | `rpc_timeout` 30s + `x-deadline` กำกับ worker | v1 ตอบ 504 ไปแล้วแต่ออเดอร์ยังถูกสร้างอีก 3 นาทีให้หลัง (ghost order) v2 worker ไม่ยิง upstream หลัง caller เลิกรอ |
| 4 | **`X-Trace-Id`** | ไม่มี | ติดมาทุก response | เก็บลง log ฝั่ง caller แล้วเทียบกับ `request_logs`/`attempt_logs` ได้ทันที ถ้า caller ส่ง `X-Trace-Id` มาเอง v2 เก็บไว้ใน `caller_trace_id` ให้ join กัน |
| 5 | **status ใหม่** | มีแค่ 200 / 403 / 504 | เพิ่ม `404` ไม่มี group นั้น, `503`+`Retry-After: 5` flow ยังไม่พร้อม, `413` body เกิน 4MB, `405` ไม่ใช่ POST | `503` ควร retry ไม่ใช่ทิ้ง |

**สรุปสำหรับ caller** ถ้าอ่าน `code` ใน body เป็นหลักและไม่ดู HTTP status เลย เปลี่ยนแค่ domain จบ
แต่ต้องเช็คสองอย่างก่อน: **IP อยู่ใน `ALLOWED_IPS` หรือยัง** และ **โค้ดตัดสินใจจาก HTTP status หรือเปล่า**

---

## 3. สถาปัตยกรรมและการทำงาน

| หัวข้อ | v1 | v2 |
|---|---|---|
| **start service** | `main.go` 13 บรรทัด เปิด HTTP อย่างเดียว **ไม่สตาร์ท consumer เลย** | reconciler อ่าน DB แล้วสร้าง flow เองตั้งแต่บูต |
| **หลัง deploy** | ต้อง `curl /consdeposit /conswithdraw /consdepositauto /conswithdrawauto` ด้วยมือ **ลืม = ฝากถอนตายเงียบ ๆ** ขณะที่ `/online` ยังตอบ 200 | ไม่ต้องทำอะไร flow ขึ้นเองภายใน 1 รอบ reconcile |
| **เพิ่ม/แก้ group** | แก้ env แล้ว deploy ใหม่ แล้ว curl ใหม่ | `INSERT`/`UPDATE` ใน DB รอไม่เกิน `RECONCILE_INTERVAL` |
| **AMQP connection** | เปิด connection ใหม่**ทุก HTTP request** แล้วปิด (`ConnectMQ`/`CloseMQ`) | connection เดียวทั้ง process ต่อใหม่เองเมื่อหลุด + channel pool |
| **เลือก upstream url** | สุ่มมา **1 ตัวแล้วจบ** ตัวนั้นล่ม = ออเดอร์ล่ม (`getRandomDepositURL`) | สับไพ่ทั้งชุดแล้วไล่ยิงทีละตัวจนสำเร็จ / เจอ fatal / หมดเวลา |
| **แยก retry ได้ไหม** | ไม่มี retry เลย | `httptrace.WroteRequest` แยก "ยังไม่ถึงปลายทาง" (retry ได้) ออกจาก "ส่งไปแล้วไม่รู้ผล" (ห้าม retry เด็ดขาด) |
| **ghost order** | เกิดได้ — caller ได้ 504 ที่ 90s แต่ worker ยังยิงต่อได้ถึง 300s | กันด้วย `x-deadline` พิสูจน์แล้ว: คิวยาวจน 16 request หมดอายุ → **ยิง upstream 0 ครั้ง** บันทึก `status='expired'` |
| **log** | มีโค้ด insert แต่ตารางว่างเปล่าใน production TLS handshake fail จึงไม่เหลือร่องรอย | `request_logs` + `attempt_logs` ผูกด้วย `trace_id` เห็นทุก url ที่ยิง ผลลัพธ์ และเวลาที่ใช้ |
| **shutdown** | ไม่มี graceful shutdown | SIGTERM → หยุดรับใหม่ → drain งานที่ค้าง → ปิด |
| **readiness** | `/online` ตอบ 200 เสมอแม้ consumer ตายหมด | `/readyz` ดูสุขภาพ AMQP จริงและ state ของทุก flow |
| **config ผิด** | `strconv.Atoi("")` ล้มกลางทาง `log.Fatalf` ฆ่าทั้ง container ตอน runtime | `config.Load` ตรวจครบแล้วตายตั้งแต่ start พร้อมบอกชื่อตัวแปรที่ขาด |
| **deploy** | `docker-compose.yaml` | `Dockerfile.gateway` multi-stage binary 7.4MB non-root uid 10001 |
| **migration** | ทำมือ | ฝังใน binary รันเองตอน start กันชนด้วย `pg_advisory_lock` |
| **test** | 0 ตัว | 138 ตัว |

---

## 4. ตาราง DB ที่ v2 เพิ่มเข้ามา

| ตาราง | ใช้ทำอะไร |
|---|---|
| `message_group` | 1 แถว = 1 flow = 1 queue = 1 endpoint พร้อม `worker_count` `upstream_timeout_ms` `rpc_timeout_ms` `ref_field` |
| `message_group_url` | url ปลายทางของแต่ละ group ปิดทีละตัวด้วย `is_active` ได้โดยไม่ deploy |
| `request_logs` | 1 แถวต่อ 1 request ที่เข้ามา มี `trace_id` `business_ref` `status` `http_status` |
| `attempt_logs` | 1 แถวต่อ 1 ครั้งที่ยิง upstream ผูกกลับด้วย `trace_id` |

```sql
-- ปิด url ที่ล่มโดยไม่ต้อง deploy
UPDATE message_group_url SET is_active = false WHERE url LIKE '%deposit-2%';
```

---

## 5. ผลทดสอบที่ยืนยันแล้วบน dev

- migration idempotent ข้าม restart 3 ครั้งได้ 1 แถวเท่าเดิม
- flow เกิดเองจาก DB ไม่ต้อง deploy / ลบ group แล้ว endpoint หายไปเอง
- failover: connection refused และ TLS handshake fail ถูกจัด retryable → caller ยังได้ 200
- HTTP 500 = fatal ไม่ retry (0 ใน 9 ครั้ง)
- `is_active=false` hot-swap ระหว่างรับงาน
- graceful shutdown ไม่ตัดงานที่ค้าง
- `x-deadline`: 16 request ที่หมดอายุยิง upstream 0 ครั้ง
- load test k6 56,519 request ไต่ถึง 146 rps (811 เท่าของพีคจริง 1.7 rps) 0 failed เวลาประมวลผลภายใน max 21ms
- ยิงรายการฝาก/ถอนจริงครบทั้ง 4 เส้น ได้ `code:0` ทุกเส้น (`scripts/live-test.sh`)

---

## 6. อ้างอิง

- spec: `docs/superpowers/specs/2026-09-25-mq-gateway-refactor-design.md`
- plan: `docs/superpowers/plans/2026-09-27-mq-gateway-v2.md`
- ขั้นตอน smoke test + rollback: `docs/smoke-test-gateway.md`
- ปริมาณ traffic จริงและการตั้ง worker: `docs/traffic-analysis.md`
- งานที่ยังค้าง 33 ข้อ: `docs/mq-gateway-v2-followups.md`
- สคริปต์ยิงของจริง: `scripts/live-test.sh`
