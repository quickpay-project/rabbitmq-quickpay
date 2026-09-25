# MQ Gateway v2 — Design

- **วันที่:** 2026-09-25
- **สถานะ:** อนุมัติแล้ว รอ implementation plan
- **Branch:** `feat/mq-gateway-v2`
- **ขอบเขต:** เขียน service ใหม่ที่ขับเคลื่อนด้วย DB มาแทน `rabbitmq-quickpay` เดิม โดยของเดิมยังรัน production ต่อไปจนกว่าจะ cutover ครบ

---

## 1. เป้าหมาย

ระบบปัจจุบันเป็น HTTP → AMQP RPC → External API gateway สำหรับงานฝาก/ถอนของ Quickpay
มี 4 flow ตายตัว (`withdraw`, `withdrawauto`, `deposit`, `depositauto`) ที่ถูก hardcode ทั้งชื่อ queue,
โครงสร้าง request/response, ปลายทาง URL และจำนวน worker

เป้าหมายของการเขียนใหม่คือทำให้ระบบ **auto และ dynamic ที่สุด** โดยแก้ 4 pain point ที่เจ็บที่สุดก่อน

| # | Pain point | ทางแก้ในดีไซน์นี้ |
|---|---|---|
| 1 | การสร้าง group และการส่ง request ต่อ ต้องเก็บบน DB | §4 Data model + §6 Reconciler |
| 2 | log ต้องตาม request ได้ | §4 `request_logs`/`attempt_logs` + §7.2 trace_id |
| 3 | deploy ด้วย Dockerfile แทน compose | §8 Deploy |
| 4 | ตัด manual curl ให้ consumer ขึ้นเองตั้งแต่ start | §6.6 Startup sequence |

---

## 2. ปัญหาของระบบปัจจุบัน (ที่ดีไซน์นี้แก้)

อ้างอิงโค้ดที่ `HEAD` ของ branch `mark_dev`

| ปัญหา | หลักฐาน | ผลกระทบ |
|---|---|---|
| consumer ไม่ auto-start | `main.go` ทั้งไฟล์มี 13 บรรทัด ไม่มีการเรียก consumer | ทุก deploy ต้อง `curl` 4 เส้นด้วยมือ ระหว่างนั้นฝาก/ถอนตายสนิทแต่ `/online` ยังตอบ 200 |
| `select {}` ปิดทางกู้ตัวเอง | `rabbitMQ/conswithdraw.go:288`, `conswithdrawauto.go:288` | AMQP หลุด → worker ออกหมด → goroutine ค้างถาวร ไม่มีใครรู้ว่าตาย |
| prefetch ไม่ตรงกับ worker | `conswithdraw.go:192` ตั้ง `Qos(10)` แต่ `:216` สร้าง worker ตาม `WITHDRAW_LIMIT=300` | throughput ถอนเงินตันที่ 10 ข้อความพร้อมกัน worker อีก 290 ตัวว่าง |
| เปิด AMQP connection ใหม่ทุก request | `deposit.go:28`, `withdraw.go:28` เรียก `ConnectMQ()` ต่อ 1 ออเดอร์ | TCP + AMQP handshake + declare queue ต่อออเดอร์ เป็นคอขวดและเสี่ยง fd หมด |
| timeout ไม่สอดคล้องกัน | RPC รอ 90s (`deposit.go:75`) แต่ HTTP client ตั้ง 300s (`consdeposit.go:183`) | caller ได้ 504 ที่วินาทีที่ 90 แต่ upstream ยังทำงานต่อ → ออเดอร์เกิดจริงโดยไม่มีใครรู้ |
| ไม่มี failover | `consdeposit.go:145-158` สุ่ม URL เดียวแล้วจบ | 1 ใน 4 URL ล่ม = 25% ของออเดอร์ fail ทันที |
| log ไล่ไม่ได้ | insert แถวเดียวตอนจบใน `deposit_logs` | request ที่ตายกลางทางไม่เหลือร่องรอยเลยสักแถว |
| env ที่ไม่มีใครอ่าน | `WITHDRAW_GROUP_STATUS`, `DEPOSIT_GROUP_STATUS`, `BALANCE_LIMIT`, `CONFIRMORDER_LIMIT` | เข้าใจผิดว่าปิด group ที่ล่มได้ ทั้งที่ทำไม่ได้ |
| `.env` อยู่ใน git | tracked ตั้งแต่ commit `2daedcd` | รหัส Postgres อยู่ใน history และถูก `COPY . .` เข้า image |
| deploy ไม่ graceful | ไม่มี `signal.Notify`, ไม่เคยเรียก `ch.Cancel()` | SIGTERM = ตายทันที unacked ถูก requeue แล้วยิงซ้ำที่ upstream |

---

## 3. ขอบเขต

### อยู่ในขอบเขต
- Service ใหม่ทั้งหมดใน `cmd/gateway` + `internal/*` ของ repo เดิม
- Schema ใหม่ 4 ตาราง พร้อม migration ที่รันเองตอน start
- Dockerfile multi-stage สำหรับ service ใหม่
- ลบ `udpsocket/` และถอด `gocv` ออกจาก `go.mod` (ดู §8.1)

### อยู่นอกขอบเขต
- Admin API / UI สำหรับจัดการ group — จัดการด้วย SQL ตรง ๆ ก่อน
- Metrics / Prometheus — ใช้ query จาก `request_logs` ไปก่อน
- Dead letter queue — ตาราง log ทำหน้าที่ audit trail แทน (§7.6)
- โหมด async + callback แทน RPC — เก็บไว้พิจารณาภายหลัง
- Idempotency key กับ upstream — ต้องคุยกับ goquickpay ก่อน (§9.1)
- Retention / partition ของตาราง log — ตารางจะโตไม่มีขีดจำกัด (ดู §9.5)
- แก้โค้ดเก่าใน `main.go`, `rabbitMQ/`, `controllers/`, `route/` — ไม่แตะเลย

---

## 4. Data model

### 4.1 Config

```sql
CREATE EXTENSION IF NOT EXISTS pgcrypto;   -- เผื่อ PG < 13

-- 1 row = 1 flow = 1 queue = 1 consumer = 1 HTTP endpoint
CREATE TABLE message_group (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    group_name          TEXT NOT NULL UNIQUE,
    worker_count        INT  NOT NULL DEFAULT 50,
    upstream_timeout_ms INT  NOT NULL DEFAULT 30000,
    rpc_timeout_ms      INT  NOT NULL DEFAULT 60000,
    ref_field           TEXT NOT NULL DEFAULT 'customer_order_id',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 1 row = 1 url ที่อยู่ใน group นั้น
CREATE TABLE message_group_url (
    id               BIGSERIAL PRIMARY KEY,
    message_group_id UUID NOT NULL REFERENCES message_group(id) ON DELETE CASCADE,
    url              TEXT NOT NULL,
    is_active        BOOLEAN NOT NULL DEFAULT true,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (message_group_id, url)
);

CREATE INDEX idx_message_group_url_parent ON message_group_url (message_group_id) WHERE is_active;
```

**หลักการที่ใช้ตัดสิน**

- `group_name` มีที่อยู่ที่เดียวคือ `message_group.group_name` ไม่เก็บซ้ำใน `message_group_url` เพื่อไม่ให้สองตารางขัดกันเองเวลามีคนเปลี่ยนชื่อ
- **ไม่มี `is_active` ระดับ group** — group ทำงานได้ถ้ามี url ที่ `is_active = true` อย่างน้อย 1 ตัว อยากปิดทั้ง flow ก็ปิด url ทุกตัว ไม่ต้องมีสองสวิตช์ให้ขัดกันเอง
- `message_group_url` มี `is_active` ระดับ url เพราะเป็นสิ่งที่ต้องเปิดปิดบ่อยที่สุด
- ค่าระดับ group อยู่แยกจาก url เพราะ `message_group_url` เป็น 1 row ต่อ 1 url ถ้ายัด `worker_count` ลงไปจะต้องเขียนค่าเดิมซ้ำทุกแถวและขัดกันได้

**Invariant ที่บังคับในโค้ด**

`upstream_timeout_ms × จำนวน url ที่ active ≤ rpc_timeout_ms`
ถ้าตั้งขัดกัน reconciler จะ log warning และยังคงทำงานต่อ โดย §7.4 จะไม่เริ่ม attempt ที่รู้ว่าจะไม่ทัน deadline อยู่แล้ว

### 4.2 Log

```sql
CREATE TABLE request_logs (
    trace_id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    message_group_id UUID,             -- ชี้ message_group.id แบบไม่ผูก FK
    group_name       TEXT NOT NULL,    -- snapshot ชื่อ ณ เวลานั้น
    status           TEXT NOT NULL,    -- pending | success | failed | no_upstream | timeout | expired
    business_ref     TEXT,             -- ดึงจาก body ตาม ref_field ของ group
    caller_trace_id  TEXT,             -- X-Trace-Id ที่ caller ส่งมา (ถ้ามี)
    client_ip        TEXT,
    request_body     JSONB,
    response_body    JSONB,
    http_status      INT,              -- ที่ตอบกลับ caller
    attempt_count    INT NOT NULL DEFAULT 0,
    total_ms         INT,
    error_message    TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at      TIMESTAMPTZ
);

CREATE TABLE attempt_logs (
    id                   BIGSERIAL PRIMARY KEY,
    trace_id             UUID NOT NULL REFERENCES request_logs(trace_id) ON DELETE CASCADE,
    seq                  INT  NOT NULL,   -- 1, 2, 3 ตามลำดับที่ลอง
    message_group_url_id BIGINT,          -- ชี้ message_group_url.id แบบไม่ผูก FK
    url                  TEXT NOT NULL,   -- snapshot url ณ เวลานั้น
    http_status          INT,
    duration_ms          INT,
    outcome              TEXT NOT NULL,   -- success | retryable | fatal
    response_body        TEXT,
    error_message        TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (trace_id, seq)
);

CREATE INDEX idx_request_logs_pending ON request_logs (created_at) WHERE status = 'pending';
CREATE INDEX idx_request_logs_ref     ON request_logs (business_ref) WHERE business_ref IS NOT NULL;
CREATE INDEX idx_request_logs_group   ON request_logs (message_group_id, created_at DESC);
CREATE INDEX idx_attempt_logs_url     ON attempt_logs (message_group_url_id, created_at DESC);
```

**หลักการที่ใช้ตัดสิน**

- `request_logs` ถูก **INSERT ตั้งแต่รับ HTTP เข้ามาด้วย `status='pending'`** แล้ว UPDATE ตอนจบ
  ทำให้ request ที่ตายกลางทางยังเหลือร่องรอย ซึ่งระบบเดิมทำไม่ได้เลย
- ตาราง log **ไม่ผูก FK ไปหา config** เพราะ log เป็นบันทึกประวัติศาสตร์ ถ้าผูกไว้จะลบ group ที่เลิกใช้ไม่ได้
  จึงเก็บ UUID ไว้สำหรับ join ตอนที่ config ยังอยู่ และเก็บ `group_name` / `url` เป็น snapshot text สำหรับตอนที่มันถูกลบไปแล้ว

**ข้อแลกเปลี่ยนที่ยอมรับ:** วิธีนี้เขียน DB 2 ครั้งต่อ request บวก 1 ครั้งต่อ attempt
ถ้าวัดแล้วหนักเกินไปค่อยเพิ่ม buffered writer ทีหลัง ยังไม่ทำตอนนี้

**ความหมายของ `status` กับ `http_status` — คนละเรื่องกัน**

| คอลัมน์ | ใครเขียน | ความหมาย |
|---|---|---|
| `status` | worker | ผลจริงของการประมวลผล |
| `http_status` | ฝั่งรับ HTTP | สิ่งที่ caller ได้รับจริง |

ค่าที่เป็นไปได้ของ `status`

- `pending` — INSERT ตอนรับ request เข้ามา ยังไม่จบ
- `success` / `failed` — worker ประมวลผลจบแล้ว
- `no_upstream` — group ไม่มี url ที่ active
- `expired` — worker ไม่ยิง upstream เลยเพราะเลย `x-deadline` ไปแล้ว (§7.3)
- `timeout` — ฝั่งรับ HTTP รอ reply ไม่ทัน `rpc_timeout_ms` แล้วตอบ caller ไปก่อน

สองฝั่งเขียนแถวเดียวกันจึงใช้ compare-and-set: ฝั่ง HTTP เขียน `timeout` ได้เฉพาะตอนที่ยังเป็น
`pending` ส่วน worker เขียนผลจริงทับได้เสมอ เพราะ worker คือความจริง

ผลที่ตามมาโดยตั้งใจ: แถวที่ `http_status = 504` แต่ `status = 'success'` คือเคสที่
**caller คิดว่าล้มเหลวแต่ออเดอร์เกิดขึ้นจริง** ซึ่งเป็นเคสอันตรายที่สุดของระบบเดิมและมองไม่เห็นเลย
ตอนนี้หาเจอตรง ๆ

```sql
SELECT trace_id, group_name, business_ref, created_at
FROM request_logs
WHERE http_status = 504 AND status = 'success';
```

ถ้า publish เข้า queue ไม่สำเร็จตั้งแต่แรก (broker ล่ม) ฝั่ง HTTP จะ UPDATE เป็น `failed` พร้อม
`error_message` แล้วตอบ 503 ไม่ปล่อยแถวค้างเป็น `pending` ตลอดไป

### 4.3 ตัวอย่าง query ที่ต้องทำได้

```sql
-- ไล่ออเดอร์เดียวว่าเกิดอะไรขึ้นบ้าง
SELECT r.group_name, r.status, a.seq, a.url, a.http_status, a.outcome, a.duration_ms
FROM request_logs r JOIN attempt_logs a USING (trace_id)
WHERE r.business_ref = 'ORDER-12345' ORDER BY a.seq;

-- request ที่ค้างอยู่ตอนนี้ ค้างมานานแค่ไหน
SELECT group_name, trace_id, now() - created_at AS stuck_for
FROM request_logs WHERE status = 'pending' ORDER BY created_at;

-- url ไหนพังบ่อยสุดวันนี้
SELECT u.url,
       count(*) FILTER (WHERE a.outcome <> 'success') AS fails,
       count(*) AS total
FROM attempt_logs a JOIN message_group_url u ON u.id = a.message_group_url_id
WHERE a.created_at > now() - interval '1 day'
GROUP BY u.url ORDER BY fails DESC;
```

---

## 5. สถาปัตยกรรมโดยรวม

Engine ตัวเดียวที่ไม่รู้จักคำว่า "deposit" หรือ "withdraw" เลย รู้จักแค่คำว่า **flow**
flow ทุกตัวเกิดจากแถวใน `message_group` เท่านั้น

```
                    ┌─ reconciler (ทุก 30 วิ) ────────────────────┐
                    │  desired = SELECT จาก DB                    │
                    │  actual  = registry ของ flow ที่รันอยู่      │
                    │  diff → START / HOT-SWAP / RESTART / STOP   │
                    └────────────────────────────────────────────┘
                                      │
POST /{group_name}                    ▼
   │  trace_id + INSERT pending            flow "withdraw"
   ├─ publish (x-trace-id, x-deadline) ──► queue {prefix}withdraw
   │                                          │
   │                                     worker pool
   │                                          │  shuffle urls แล้วยิงตามลำดับ
   │                                          │  ล้มแบบปลอดภัย → ตัวถัดไป
   │                                          │  INSERT attempt_logs ทุกครั้ง
   │                                          │  UPDATE request_logs ตอนจบ
   └────────── reply (correlation_id) ◄───────┘
```

**Passthrough ทั้งหมด** — body และ response ถูกส่งต่อดิบ ๆ ไม่ parse ไม่ validate
นี่คือเงื่อนไขบังคับที่ทำให้เพิ่ม flow ใหม่ได้โดยไม่ต้องแก้โค้ด และเป็นเหตุผลที่ struct
`DepositResponse` / `WithdrawResponse` แบบเดิมหายไปทั้งหมด

### 5.1 การวางแพ็กเกจ

```
main.go, rabbitMQ/, controllers/, route/    ← ของเก่า ไม่แตะเลยสักบรรทัด
cmd/gateway/main.go                         ← ของใหม่: wiring อย่างเดียว
internal/config/     อ่าน env + validate
internal/store/      groups, logs, migrations
internal/amqpx/      connection manager, channel pool, RPC client
internal/flow/       Flow + registry + lifecycle
internal/reconcile/  diff + apply
internal/forward/    HTTP forwarder + error classification
internal/httpapi/    dynamic router, healthz/readyz
internal/trace/      trace_id
```

สองระบบอยู่ร่วม repo ได้เพราะคนละ build target — ของเก่า `go build .` ของใหม่ `go build ./cmd/gateway`

---

## 6. Runtime

### 6.1 Connection model

```
1 process
 └── 1 amqp.Connection (auto-reconnect พร้อม backoff)
      ├── consumer channel ต่อ flow   (Qos เป็นค่าระดับ channel จึงต้องแยกต่อ flow)
      └── RPC client channel pool (default 4 ช่อง)
           └── แต่ละช่อง: consume amq.rabbitmq.reply-to ครั้งเดียวตอน start
                          + map[correlation_id] → chan []byte
```

RPC client pool consume `amq.rabbitmq.reply-to` ครั้งเดียวตอน start แล้ว dispatch reply เข้า
goroutine ที่รออยู่ด้วย correlation_id — ต่อ 1 ออเดอร์เหลือแค่ publish 1 ครั้ง
ที่ต้องเป็น pool ไม่ใช่ช่องเดียวเพราะ `amqp091-go` ล็อกภายในตอน publish ช่องเดียวจะ serialize ทั้งระบบ

### 6.2 Flow

```go
// 1 Flow = 1 group = 1 queue + 1 consumer + worker pool + 1 HTTP route
type Flow struct {
    spec   atomic.Pointer[GroupSpec]  // hot-swap ได้โดยไม่ต้องรีสตาร์ท
    status Status                      // starting|running|degraded|draining|failed
    cancel context.CancelFunc
    done   chan struct{}
}
```

`spec` เป็น atomic pointer เพราะรายการ URL เปลี่ยนได้ตลอดเวลาโดยที่ worker ไม่ต้องหยุด
worker อ่าน spec ใหม่ทุกครั้งที่หยิบ message

### 6.3 Reconciler

ทุก `RECONCILE_INTERVAL` โหลด desired state ด้วย query เดียว แล้วเทียบกับ registry

```sql
SELECT g.id, g.group_name, g.worker_count, g.upstream_timeout_ms, g.rpc_timeout_ms, g.ref_field,
       u.id AS url_id, u.url, u.is_active
FROM message_group g
LEFT JOIN message_group_url u ON u.message_group_id = g.id
ORDER BY g.group_name, u.id;
```

| สภาพ | การกระทำ |
|---|---|
| มีใน DB ไม่มีใน registry | **START** — declare queue → เปิด channel + Qos → worker pool → เปิดรับ route |
| revision เท่ากัน | ไม่ทำอะไร |
| URL list หรือ `ref_field` เปลี่ยน | **HOT-SWAP** — สลับ `spec` pointer อย่างเดียว ไม่แตะ consumer |
| `worker_count` หรือ `group_name` เปลี่ยน | **RESTART** — drain ให้จบก่อนแล้วเปิดใหม่ (Qos ผูกกับ channel / ชื่อ queue เปลี่ยน) |
| หายไปจาก DB | **STOP** — drain แล้วปิด |
| ยังอยู่แต่ไม่มี url ที่ active | **DEGRADED** — route ตอบ 503, ข้อความที่ค้างถูก fail ทันทีพร้อม log `no_upstream` |

`revision` = hash ของ spec ทั้งก้อน ทำให้เกือบทุกรอบจบที่ "ไม่ทำอะไร" โดยไม่ต้องเทียบทีละ field

**เคสเปลี่ยนชื่อ group:** เพราะ key เป็น UUID ไม่ใช่ชื่อ reconciler จึงเห็นว่าเป็น group เดิมที่ย้ายบ้าน
ไม่ใช่ลบแล้วสร้างใหม่ → drain คิวเก่าให้หมดก่อนค่อยเปิดคิวใหม่ ถ้า key เป็นชื่อ ข้อความในคิวเก่าจะหายทันที

### 6.4 Drain

```
1. ch.Cancel(tag)        → broker หยุดส่งของใหม่ msgs จะปิดเองหลัง delivery สุดท้าย
2. status = draining     → route ตอบ 503 ไม่มี publish ใหม่เข้า flow นี้
3. worker ทำของในมือจนจบ  → Ack ตามปกติ
4. wg.Wait() มี deadline → rpc_timeout + 5s
5. เลย deadline          → log ชัดเจน แล้วปิด channel (unacked ถูก requeue โดยที่เรารู้ตัว)
6. ปิด channel ถอดออกจาก registry
```

ระบบเดิมไม่เคยเรียก `ch.Cancel()` เลยสักที่ ทำให้ตอน shutdown consumer ยังดูดงานใหม่เข้ามาแล้วโดนฆ่ากลางทาง

### 6.5 Self-healing

แต่ละ flow มี supervisor ของตัวเอง เฝ้า `ch.NotifyClose` และการ return ของ consumer loop
ถ้าจบแบบไม่ได้ตั้งใจ → mark `failed` → reconciler รอบถัดไปเปิดใหม่ด้วย exponential backoff

> **กฎบังคับ: consumer loop ต้อง return เสมอเมื่อ `msgs` ปิด ห้ามมี `select {}` หรือ block ถาวรเด็ดขาด**
> นี่คือบั๊กที่ทำให้ `conswithdraw.go:288` กู้ตัวเองไม่ได้และ `/readyz` รายงานเขียวทั้งที่ตายไปแล้ว
> ต้องมี test ที่ปิด channel แล้วยืนยันว่า flow กลับมาเองได้

### 6.6 Startup sequence

```
1. อ่าน env → ขาดตัวบังคับตัวไหน ตายทันทีพร้อมบอกชื่อตัวแปร
2. รัน migration ที่ฝังใน binary (advisory lock กันหลาย replica ชนกัน)
3. ต่อ DB + ping
4. ต่อ RabbitMQ (retry backoff ไม่ panic)
5. เปิด HTTP server ก่อน       ← /healthz ตอบได้ระหว่างบูต, /readyz ยัง 503
6. reconcile รอบแรกแบบ sync    ← flow ทุกตัวขึ้นตรงนี้ ไม่ต้อง curl อะไรทั้งนั้น
7. /readyz เขียวเมื่อ flow ที่ควรรัน รันครบ
8. reconcile loop ทำงานเบื้องหลัง
```

flow ตัวใดตัวหนึ่ง start ไม่ขึ้นจะไม่บล็อกตัวอื่น แต่ `/readyz` รายงานเป็นรายตัวว่าใครไม่ขึ้นเพราะอะไร

### 6.7 Shutdown

SIGTERM → หยุด reconciler → `srv.Shutdown` → drain ทุก flow ขนานกัน → รอจนถึง
`GRACEFUL_TIMEOUT` (default 45s) → ปิด connection → exit

ตอน start จะ log warning ถ้า `max(rpc_timeout_ms) + 10s > GRACEFUL_TIMEOUT`
เพราะระบบเดิมต้องใช้ 30 วิแต่ docker ให้ 10 วิ ทำให้ graceful shutdown ไม่เคยทำงานจบสักครั้ง

---

## 7. Request path

### 7.1 Dynamic routing

ไม่ลงทะเบียน route เข้า mux ตอน runtime (mux ไม่ปลอดภัยต่อการเขียนพร้อมอ่าน)
ใช้ handler ตัวเดียวที่ lookup registry แทน

```
POST /{group_name}
  ├─ registry ไม่มี group นี้       → 404
  ├─ มี แต่ starting/draining      → 503 + Retry-After
  ├─ มี แต่ degraded (ไม่มี url)    → 503 พร้อมเหตุผล
  └─ running                      → publish path
```

`group_name` ถูก validate ตอน reconcile ด้วย `^[a-z0-9][a-z0-9_-]{0,63}$` และห้ามชนคำสงวน
(`healthz`, `readyz`) ผิดกติกาจะถูกข้ามพร้อม log ไม่ทำให้ระบบล้ม

### 7.2 trace_id

```
POST /withdraw
   │  trace_id = uuid ที่เราสร้างเองเสมอ
   │  X-Trace-Id ที่ caller ส่งมา → เก็บเป็น caller_trace_id เฉย ๆ ไม่เอามาเป็น PK
   ├─ INSERT request_logs (status='pending')
   ├─ publish headers: x-trace-id, x-deadline, + header ของ caller
   ↓
worker
   ├─ INSERT attempt_logs ทุกครั้งที่ยิง
   ├─ UPDATE request_logs (status, total_ms, response_body, attempt_count)
   ↓
HTTP response + Header: X-Trace-Id
```

### 7.3 `x-deadline`

ตอน publish คำนวณ `x-deadline = now + rpc_timeout_ms` ใส่ไปกับข้อความ worker ทุกตัวเคารพค่านี้

```
worker หยิบข้อความขึ้นมา:
   ถ้า now > deadline → ไม่ยิง upstream เลย
                      → UPDATE request_logs status='expired'
                      → Ack ทิ้ง ไม่ต้อง reply (ไม่มีใครรออยู่แล้ว)
```

แก้ปัญหาที่ระบบเดิมมี: ข้อความที่ค้างในคิวตอน consumer ตายจะถูกยิงย้อนหลังตอน consumer กลับมา
ทั้งที่ caller ได้ 504 ไปนานแล้ว กลายเป็นออเดอร์จริงที่ไม่มีใครรู้
และบังคับความสัมพันธ์ระหว่าง upstream timeout กับ RPC timeout ให้เองโดยไม่ต้องพึ่งวินัยคน

### 7.4 การเลือก URL และ failover

```
1. urls = spec.URLs (เฉพาะ is_active)   ว่าง → no_upstream, log, ตอบ 503
2. shuffle                              → กระจายโหลดเท่า ๆ กัน
3. for seq, url := range urls:
       เวลาที่เหลือ < upstream_timeout ? → หยุด ไม่เริ่มครั้งที่รู้ว่าไม่ทัน
       ยิง POST (timeout = upstream_timeout)
       INSERT attempt_logs
       success → จบ | fatal → จบ | retryable → ลองตัวถัดไป
```

### 7.5 ตารางจำแนก error

หลักการเดียว: **ลองใหม่เฉพาะเมื่อมั่นใจว่าคำขอไปไม่ถึงแอปปลายทาง**

| ผลลัพธ์ | ประเภท | เหตุผล |
|---|---|---|
| 2xx | success | จบ |
| DNS fail / connection refused | retryable | ต่อไม่ติดตั้งแต่แรก แอปไม่เคยเห็นคำขอ |
| TLS handshake fail | retryable | ยังไม่ได้ส่ง body |
| 502 Bad Gateway | retryable | proxy ต่อ backend ไม่ได้ |
| 503 Service Unavailable | retryable | ปลายทางปฏิเสธตั้งแต่ต้น |
| 429 Too Many Requests | retryable | ถูกปฏิเสธก่อนประมวลผล |
| 404 | retryable | URL ผิดหรือย้าย ลองตัวอื่นในกลุ่ม |
| 400 / 401 / 403 / 422 | fatal | คำขอผิดเอง ลองกี่ครั้งก็ผิดเหมือนเดิม |
| **504 Gateway Timeout** | **fatal** | proxy ต่อแอปได้แล้วแต่แอปไม่ตอบ — แอปอาจสร้างออเดอร์ไปแล้ว |
| **500 Internal Server Error** | **fatal** | แอปรับคำขอไปแล้วและพังระหว่างทาง อาจสร้างไปบางส่วน |
| **client timeout ระหว่างรอ response** | **fatal** | ส่ง body ไปแล้ว ไม่รู้ผล ลองต่อเสี่ยงทำซ้ำ |
| connection reset หลังส่ง body | **fatal** | เหตุผลเดียวกัน |

ตารางนี้ fix ในโค้ดพร้อมคอมเมนต์เหตุผล **ไม่ทำให้ config ได้** เพราะเป็นเรื่องความถูกต้องทางธุรกิจ ไม่ใช่การปรับจูน

### 7.6 Ack strategy และ durability

- **Ack เสมอหลังประมวลผลเสร็จ ไม่ requeue** — caller รอแบบ synchronous การ requeue จะสร้าง
  ออเดอร์ซ้ำให้คนที่เดินจากไปแล้ว ความล้มเหลวถูกบันทึกครบใน `request_logs` + `attempt_logs`
- **ไม่ทำ DLQ** — ตาราง log คือ audit trail
- worker panic → recover → log `failed` → Ack (ไม่ปล่อยให้ poison message วนไม่รู้จบ)
- **queue: `durable = true`** — ให้ตัวนิยามคิวรอดข้าม broker restart (ถูกมาก ไม่มี fsync ต่อข้อความ)
- **message: ไม่ persistent** — ข้อความหายตอน broker restart ถือว่าถูกต้อง เพราะถ้ารอดมาก็ expired ตาม §7.3 อยู่ดี

### 7.7 Passthrough และ header

- body: ส่งต่อดิบ ไม่ parse ไม่ validate
- response: ส่งกลับ caller ดิบพร้อม status code เดิมจาก upstream
- header ไป upstream: ส่งต่อทั้งหมด ยกเว้น hop-by-hop (`Host`, `Connection`, `Content-Length`, `Transfer-Encoding`)
- `business_ref`: อ่านจาก body ตาม `ref_field` ของ group หาไม่เจอปล่อย null ไม่ error

### 7.8 IP whitelist

- `ALLOWED_IPS` เป็น env ตัวเดียว บังคับกับทุก group เท่ากันหมด
- ค่าว่าง = ปิดทุกคน (fail-closed) จะเปิดต้องเขียน `ALLOWED_IPS=*` ชัดเจน
- ดึง client IP โดยนับ proxy ด้วย `TRUSTED_PROXY_COUNT` (default 1) แทนการเชื่อ
  `X-Forwarded-For` ตัวซ้ายสุดซึ่ง caller ปลอมได้ — ระบบเดิมใช้ตัวซ้ายสุด (`controllers/homeController.go:72-75`)

---

## 8. Deploy และ setup

### 8.1 Dockerfile

```dockerfile
# ---- build ----
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOTOOLCHAIN=local \
    go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

# ---- runtime ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 app
COPY --from=build /out/gateway /usr/local/bin/gateway
USER app
EXPOSE 4000
ENTRYPOINT ["/usr/local/bin/gateway"]
```

| ของเดิม | ของใหม่ |
|---|---|
| single-stage ติด toolchain + source ทั้งก้อน | multi-stage เหลือ binary ~15MB |
| base 1.21 แต่ go.mod สั่ง `toolchain go1.23` → โหลด toolchain ทุก build | base 1.23 + `GOTOOLCHAIN=local` |
| `COPY . .` ไม่มี `.dockerignore` → `.env` ติดเข้า image | เพิ่ม `.dockerignore` (`.env`, `.git`, `.DS_Store`, `*.md`) |
| `wait-for-rabbitmq.sh` + `nc` | ไม่ต้องมี แอป retry เองพร้อม backoff |
| รันเป็น root | user `app` (uid 10001) |
| ไม่มี CA certs | `ca-certificates` + `tzdata` (จำเป็นเพราะ DSN ตั้ง `TimeZone=Asia/Bangkok`) |

**ข้อยกเว้นเรื่องไม่แตะโค้ดเก่า:** ลบ `udpsocket/` และถอด `gocv` ออกจาก `go.mod`
เป็นโค้ด webcam จาก template ต้นทางที่มี `func main()` ซ้อนกันสองตัวจนทำให้ `go build ./...` พังอยู่ทุกวันนี้
ไม่ได้อยู่ใน build target ของของเก่า (`go build .`) จึงลบได้โดย production ไม่รู้สึกอะไร และทำให้ CI เป็นไปได้

### 8.2 Environment

```bash
# บังคับ
RABBITMQ_URL=amqp://user:pass@rabbitmq:5672/
DATABASE_URL=host=... dbname=mqv2 ...
QUEUE_PREFIX=v2.                # ห้ามว่างระหว่างที่ของเก่ายังรัน
ALLOWED_IPS=1.2.3.4,5.6.7.8     # ว่าง = ปิดทุกคน, "*" = เปิดหมด

# มี default
PORT=4000
RECONCILE_INTERVAL=30s
RPC_CHANNEL_POOL=4
GRACEFUL_TIMEOUT=45s
TRUSTED_PROXY_COUNT=1
MIGRATE_ON_START=true
```

หายไปเพราะย้ายเข้า DB: `WITHDRAW_LIMIT`, `DEPOSIT_LIMIT`, `*_URL_GROUP` 4 ตัว,
`*_GROUP_STATUS` 2 ตัว, `BALANCE_LIMIT`, `CONFIRMORDER_LIMIT`

ไม่มี lib โหลด `.env` ในโปรเจกต์นี้ (ตรวจแล้วใน `go.mod`) env ต้องมาจาก Easypanel เท่านั้น

### 8.3 การแยกออกจากระบบเดิม

RabbitMQ มีตัวเดียว จึงต้องกันชนด้วย 2 ชั้น

1. **`QUEUE_PREFIX`** — queue จริงชื่อ `{prefix}{group_name}` เช่น `v2.withdraw` บังคับในโค้ดว่าห้ามว่าง
2. **vhost แยก** (ถ้ามีสิทธิ์ admin บน broker) — `amqp://.../v2` ทับอีกชั้น

ถ้าไม่กัน consumer ใหม่จะแย่งกินออเดอร์จริงของ prod ทันทีแบบ round-robin ประมาณ 50%
โดยไม่มี error ให้เห็น เพราะ reply ยังส่งกลับได้สำเร็จบน broker เดียวกัน

DB ก็ต้องแยก (`dbname` คนละตัว) และ `message_group_url.url` ระหว่างพัฒนาต้องชี้ sandbox ไม่ใช่ endpoint จริง

### 8.4 ตั้งค่าฝั่ง Easypanel

| ตั้งอะไร | ค่า | เพราะอะไร |
|---|---|---|
| Health check path | `/readyz` | `/online` ของเดิมตอบ 200 แม้ consumer ตายหมด |
| Stop timeout | ≥ 60s | graceful ใช้ถึง 45s ถ้าให้ 10s ตาม default จะโดน SIGKILL ทุกครั้ง |
| Port | 4000 | |

### 8.5 แผน cutover ทีละ flow

```
ระยะที่ 1  ระบบใหม่ขึ้น QUEUE_PREFIX=v2. + DB แยก + group ชี้ sandbox
           ของเก่ารับ traffic จริงทั้งหมดต่อไป

ระยะที่ 2  เทสทีละ flow → เปลี่ยน url ใน message_group_url เป็น endpoint จริง

ระยะที่ 3  ที่ proxy: route /withdraw → service ใหม่ ที่เหลือยังไปของเก่า
           ดู request_logs ของ flow นั้น ถ้าไม่ดี route กลับได้ในไม่กี่วินาที

ระยะที่ 4  ย้ายครบทุก flow → ปิดของเก่า → ลบ QUEUE_PREFIX ทิ้งได้
```

rollback ง่ายเพราะไม่มี state ค้างระหว่างสองระบบ แต่ละระบบมีคิวและ DB ของตัวเอง

---

## 9. ความเสี่ยงที่ยังเหลือ

### 9.1 ออเดอร์สถานะไม่แน่นอน
เคส 504 / 500 / client timeout เราเลือกไม่ retry เพื่อกันซ้ำ แปลว่ามีออเดอร์ที่ไม่รู้ว่าเกิดหรือไม่เกิด
ระบบ log ไว้ชัดเจน (`status='failed'` + attempt ที่ `outcome='fatal'`) แต่ยังต้องมีคนไปกระทบยอดกับ upstream
ทางแก้จริงคือ idempotency key ซึ่งต้องคุยกับ goquickpay — อยู่นอกขอบเขตรอบนี้

### 9.2 Write amplification ของ log
2 writes ต่อ request + 1 ต่อ attempt ถ้า throughput สูงต้องวัดแล้วพิจารณา buffered writer

### 9.3 Engine เดียวล่ม = ล่มทุก flow
ยอมรับในรอบนี้เพื่อความง่ายของ ops การแยกเป็น API service + Worker service ทำทีหลังได้
โดยไม่ต้องรื้อ เพราะขอบเขตแพ็กเกจแยกไว้แล้ว

### 9.4 ความลับที่รั่วไปแล้ว
`.env` ที่มีรหัส Postgres อยู่ใน git history ตั้งแต่ commit แรก ต้อง rotate รหัสและ
`git rm --cached .env` แยกต่างหากจากงานนี้

---

### 9.5 ตาราง log โตไม่มีขีดจำกัด
`request_logs` และ `attempt_logs` ยังไม่มีนโยบายลบหรือ partition ในรอบนี้
ถ้า throughput สูงต้องวางแผน partition ตาม `created_at` หรือ job ลบย้อนหลังแยกต่างหาก
ตัวเลขคร่าว ๆ ที่ต้องเฝ้า: 1 request = 1 แถวใน `request_logs` + 1 แถวต่อ attempt ใน `attempt_logs`

---

## 10. บันทึกการตัดสินใจ

| # | ตัดสินใจ | ทางเลือกที่ไม่เอา | เหตุผล |
|---|---|---|---|
| 1 | `group_name` = flow = queue = consumer = endpoint สร้างอัตโนมัติจาก DB | คง 4 flow ไว้ในโค้ด | ต้องการเพิ่ม flow โดยไม่ deploy |
| 2 | Hot reload แบบ reconciler | อ่านครั้งเดียวตอน start | เปลี่ยน config ต้องมีผลโดยไม่ restart |
| 3 | Failover เฉพาะ error ที่ปลอดภัย | retry ทุกกรณี / ไม่ retry เลย | กันออเดอร์ซ้ำในงานการเงิน |
| 4 | 2 ตาราง log ผูกด้วย trace_id | ตารางเดียว / JSONB array | ต้องไล่ได้ว่าข้ามมาจาก URL ไหนเพราะอะไร |
| 5 | Generic pipeline binary เดียว | แยก API/Worker, async + callback | pain point ที่เลือกแก้ไม่ต้องการทั้งสองอย่าง และไม่ปิดทางทำทีหลัง |
| 6 | branch ใหม่ใน repo เดิม + service แยก + prefix/vhost แยก | repo ใหม่ / binary เดียวคุมด้วย flag | ของเก่าต้องรันต่อโดยไม่เสี่ยง |
| 7 | `message_group` เป็น entity มี UUID, `message_group_url` อ้างอิงกลับ | group_name เป็น key | รองรับการเปลี่ยนชื่อ group และ join log ได้ถูกต้อง |
| 8 | IP whitelist เป็น global env | เก็บใน DB ต่อ group / ให้ proxy จัดการ | ง่าย คาดเดาได้ และปิดช่องโหว่ที่ 3 เส้นถูกคอมเมนต์ทิ้ง |
