# MQ Gateway v2 — งานที่ยังค้าง

สรุปจากรีวิว 13 task + รีวิวทั้ง branch ตอนพัฒนา (2026-09-27)
ทุกข้อในนี้ **จงใจไม่แก้** ในรอบแรก ไม่ใช่ตกหล่น

---

## ต้องทำก่อน cutover (spec §8.5 ระยะที่ 3)

1. **ยืนยันกับ caller ว่ารับ HTTP status ที่เปลี่ยนไปได้**
   ระบบเก่าตอบ 200 เสมอพร้อม body ของ upstream (`controllers/homeController.go:251`)
   caller จึงน่าจะอ่านฟิลด์ `code` ใน JSON ไม่ใช่ status ระบบใหม่ส่ง status ของ upstream
   กลับไปตาม spec §7.7 — caller ที่เช็ค `if status == 200` จะพังทันที

2. **รัน integration test กับ DB และ broker จริง**
   `internal/store` มี 0 test ที่เคยทำงาน (ทั้งหมดอยู่หลัง build tag `integration`)
   `internal/amqpx` ก็เช่นกัน โค้ดคอมไพล์ผ่าน `go vet -tags=integration` แล้ว
   ขาดแค่ environment — `TEST_DATABASE_URL` กับ `TEST_RABBITMQ_URL`

3. **ทำตาม `docs/smoke-test-gateway.md` ให้ครบ โดยเฉพาะขั้นตอน 0 กับ 6**
   (นับ consumer ของ queue เดิมก่อน-หลัง เพื่อพิสูจน์ว่าไม่ไปแตะ prod)

4. **rotate รหัส Postgres แล้วถอด `.env` ออกจาก git**
   `.env` ถูก track ตั้งแต่ commit แรก (`2daedcd`) การเพิ่มใน `.gitignore` ไม่ได้เอาออกจาก history

---

## ควรทำเร็ว ๆ นี้ (ไม่บล็อก merge แต่เป็นของจริง)

- **RPC pool ไม่ auto-reconnect** — ตอนนี้แค่ทำให้ `/readyz` บอกความจริงเพื่อให้ pod ถูก restart
  ทางแก้จริงคือ `NotifyClose` + เปิด channel ใหม่ ดู `TODO(auto-reconnect)` ใน `internal/amqpx/conn.go`
  (`amqpChannel` interface ทำให้ทดสอบได้โดยไม่ต้องมี broker)
- **`Healthy()` มองไม่เห็น channel-level death** — ถ้า channel ของ pool ตายเดี่ยว ๆ แต่ connection ยังอยู่
  `/readyz` จะยังเขียวทั้งที่ `Call` 504 ตลอด
- **`HotSwap` ปลุก flow ที่ตายแล้วให้เป็น running ได้** (`internal/reconcile/loop.go:71` ยังใช้ `SetState` ธรรมดา)
  ตอนนี้มี `SetStateIf` แล้ว ปิดช่องนี้ได้ง่าย
- **ไม่มี join ของ `loop.Run` ก่อน `drainAll`** — reconcile tick ที่ค้างอยู่อาจ start flow ใหม่หลัง `loopCancel`
- **`attempt แรกไม่ติด MinAttemptBudget`** (`internal/forward/forwarder.go:87`) เบี่ยงจาก spec §7.4
  ตอน queue backlog อาจยิงด้วย context ~0ms แล้วได้ `fatal` ปลอม
- **ไม่มีการ clamp ตัวเลขจาก DB** — `worker_count = 100000` จะ spawn goroutine แสนตัวและ `Qos(100000)`
  โดยไม่ต้อง deploy และไม่มีใครรีวิว ควรมี `CHECK` constraint + clamp ใน factory
- **`stop()` บล็อก reconcile loop ได้ถึง `GracefulTimeout`** ขณะที่ interval คือ 30s — drain แบบขนาน
- **`pg_advisory_lock` ไม่มี timeout** — ถ้า key ถูกถือค้าง `Migrate` จะแฮงก์ก่อน HTTP server ขึ้น
  `/healthz` จะไม่ตอบเลยและ deploy ดูเหมือนตายโดยไม่มี log
- **`log.Fatalf` ใน goroutine ของ `ListenAndServe`** ข้าม `defer` ทุกตัวใน `main`
- **`/readyz` ตอบ 200 ตอน boot** ทั้งที่ spec §6.6 บอกว่าควรเป็น 503 จนกว่า flow จะขึ้นครบ
  (`AllRunning` บน registry ว่างคืน true)
- **reply ของทุก worker ใช้ channel เดียวกัน** — `amqp091` ล็อกต่อ publish ที่ `worker_count=500`
  reply จะ serialize ทั้งหมด (spec §6.1 ทำ RPC client เป็น pool ด้วยเหตุผลนี้)
- **`.dockerignore` ใหม่มีผลกับ image ของระบบเก่าด้วย** — เป็นการปรับปรุง (กัน `.env` หลุดเข้า image)
  แต่ deploy ครั้งหน้าของระบบเก่าคือครั้งแรกที่ build ภายใต้ไฟล์นี้ ควรยืนยันอย่างตั้งใจ

---

## เก็บไว้ทีหลังได้

- Task 1: minor (deferred): Revision() ต่อ field ด้วย | และ ; โดยไม่ escape — ค่าที่มี delimiter
        อาจชนกันได้ในทางทฤษฎี (ค่ามาจาก DB ภายใน ไม่ใช่ input ของ attacker) มาจากโค้ดใน plan เอง
- Task 1: minor (deferred): test ของ Revision() ครอบแค่ WorkerCount กับ URL set —
        RefField/UpstreamTimeout/RPCTimeout/ID ยังไม่มี test ทั้งที่มีผลต่อการตัดสิน restart
- Task 1: complete (commits bd76cfa..09efae3, review clean)
- Task 2: minor (deferred): boolean() รับค่าอะไรก็ได้ที่ไม่ใช่ false/0/no เป็น true — พิมพ์ผิด
        (MIGRATE_ON_START=fasle) จะกลายเป็น true เงียบ ๆ ต่างจาก parser ตัวอื่นในไฟล์เดียวกันที่ reject
- Task 2: complete (commits 09efae3..934eaf4, review clean)
- Task 3: minor (deferred): ไม่มี test ที่ยิง status นอกลิสต์แบบ arbitrary (418/599) เพื่อ pin
        default fallthrough ว่าเป็น fatal — ตอนนี้พึ่ง 301/4xx ที่ลงทางเดียวกัน
- Task 3: complete (commits 934eaf4..9b60f6d บน wt/forward, review clean)
- Task 8: minor (deferred): int(p.next.Add(1))%len() อาจได้ index ติดลบหลัง ~2^63 ครั้ง (เชิงทฤษฎี)
- Task 8: minor (deferred): Manager.connection ถือ mutex ตลอด retry loop รวม sleep — คนเรียก Channel()
        พร้อมกันจะต่อคิวหลัง reconnect ที่กำลังหน่วงอยู่ (มาจากโค้ดใน plan เอง กันแห่ redial)
- Task 6: minor (deferred): เช็ค urlID.Valid && urlStr.Valid ทั้งที่มาจากแถว join เดียวกัน เช็คตัวเดียวพอ
- Task 6: minor (deferred): ไม่มี test ที่มี group ที่มี url และ group ที่ไม่มี url อยู่ในผลลัพธ์ชุดเดียวกัน
- Task 6: complete (commits fafa812..f8bb60d บน wt/store, review clean)
- Task 9: review ❌ Needs fixes — [Critical] Drain ที่มาถึงก่อน Run publish consumer tag จะข้าม Cancel
- Task 5: minor (deferred): testDB ต่อ DSN ด้วย string concat (base+" search_path=") ใช้ได้เฉพาะ
        keyword=value form ไม่รองรับ postgres:// URL — fail ดัง ไม่ใช่เงียบ
- Task 5: minor (deferred): error wrapping ไม่สม่ำเสมอ บางจุด wrap บางจุดคืน bare error
- Task 5: minor (deferred): advisoryLockKey เป็น magic number ไม่มีที่มา
- Task 5: complete (commits 934eaf4..fafa812 บน wt/store, review clean)
- Task 4: reviewer diff โค้ดกับ brief แบบ byte-for-byte แล้วประเมินโค้ดเองแทนที่จะเชื่อว่า brief ถูก
- Task 4: minor (deferred): MinAttemptBudget ไม่กัน attempt แรก ถ้า deadline เหลือไมโครวินาทีก็ยังยิง
- Task 4: minor (deferred): เช็ค scheme แบบ case-sensitive — HTTP:// ใน DB จะถูกตัดเป็น malformed
- Task 4: minor (deferred): ส่ง wrote.Load() เข้า Classify บน success path ทั้งที่ไม่ถูกอ่าน
- Task 4: complete (commits 9b60f6d..1b9aeab บน wt/forward, review clean)
        → wt/forward รีวิวผ่านครบทั้ง track พร้อม merge
- Task 7: minor (deferred): import database/sql ค้างไว้เพื่อบรรทัด var _ = sql.ErrNoRows เท่านั้น (พ่วงให้แก้)
- Task 7: minor (deferred): MarkClientOutcome เขียน finished_at ซึ่งไม่อยู่ในสิทธิ์ของฝั่ง HTTP ตามตาราง
        ownership — ปลอดภัยเพราะ worker เขียนทับทีหลังเสมอ แต่หลุดจาก contract ที่เขียนไว้
- Task 7: minor (deferred): test ของ BeginRequest/FinishRequest เช็คแค่ status ไม่ได้อ่าน
        response_body/attempt_count/total_ms/error_message กลับมายืนยัน
- Task 7: fix round 1/5 ส่งกลับ pane 3 (implementer เดิม)
- Task 9: minor (deferred): Drain ไม่ idempotent และไม่มีเคส "ยังไม่เคยรัน Run"
- Task 9: minor (deferred): fakeBroker.DeclareQueue อ่าน failDecl นอก mutex
- Task 8: fix round 1/5 ผล re-review (1 addressed, 0 open, ไม่มี breakage ใหม่) —
        implementer เพิ่ม amqpChannel interface เป็น seam ให้ทดสอบ error path ได้โดยไม่ต้องมี broker
- Task 9: minor (deferred) ⚠️ ชี้ให้ final review triage เป็นอันดับแรก: การ de-dup Cancel และ
        watchdog path **ไม่มี test ครอบเลยสักตัว** — ไม่มี test ไหนนับจำนวนครั้งที่เรียก Broker.Cancel
        และไม่มี test ไหน cancel ctx เลย ประกอบกับ fakeBroker.Cancel เองก็ idempotent
- Task 9: minor (deferred): Cancel ที่ล้มถูก latch ไว้แล้วไม่ retry อีก และ watchdog กลืน error ทิ้ง
- Task 9: minor (deferred): guard tag != "" อยู่นอก flag set ใน Run post-Consume ต่างจากอีกสองจุด
- Task 9: minor (deferred): TestRunRejectsSecondCallAfterStartupFailure fail แบบ hang ไม่ใช่ assertion

หมายเหตุปฏิบัติการ: สัญญาณ "[agentspace] pane N appears finished (quiet 30s)" ไม่น่าเชื่อถือ
- Task 10: minor (deferred) ⚠️ ส่งต่อให้ Task 13: Processor.Logf default เป็น no-op และยังไม่มีใคร
         ผูกตัวจริงให้ ผลคือ double panic บน production จะ "เงียบสนิท" — ไม่ crash ไม่มี log
         มีแค่ caller ที่ไม่ได้คำตอบ plan ของ Task 13 ตั้ง proc.Logf = log.Printf ไว้แล้ว
- Task 11: minor (deferred): loop.go ทั้งไฟล์ไม่มี test เลย (Once/apply/start/stop/Run)
         invariant เรื่อง Put ก่อนสตาร์ท goroutine และการกัน zombie state ตอน Restart
         จึงไม่มีอะไรคุ้มกัน — brief ไม่ได้สั่งให้มี แต่ควรมีเป็นงานต่อเนื่อง
- Task 11: minor (deferred): Reason บอกแค่ worker_count เมื่อทั้ง worker_count และ name เปลี่ยนพร้อมกัน
- Task 11: minor (deferred): ByName เป็น linear scan ใต้ RLock บน HTTP path
- Task 11: fix round 1/5 ส่งกลับ pane 3
Ruling 14: pane 3 ไปรัน gofmt -w internal/model/group_test.go เองระหว่างแก้ Task 11
- Task 11: minor (deferred): loop_test.go ที่เพิ่มมาใหม่ยังไม่ได้ pin invariant สองข้อที่สำคัญที่สุด
         (Put ก่อนสตาร์ท goroutine, และ stale goroutine ต้องไม่ resurrect state ของ id ที่ถูกลบแล้ว)
         เพราะทั้งสอง test เดินเส้นทางที่ Factory error ตลอด ไม่เคยไปถึงจุด Put — implementer
- Task 12: minor (deferred): X-Forwarded-For อ่านด้วย Header.Get ได้เฉพาะ instance แรก
         ถ้ามี intermediary ที่ append เป็นบรรทัดที่สองแทนที่จะต่อท้ายบรรทัดเดิม จะเป็นช่องโหว่เชิงทฤษฎี
- Task 12: minor (deferred): readyz เส้นทาง 200 (ready=true) ไม่มี test
- Task 12: minor (deferred): MarkClientOutcome ใช้ r.Context() ถ้า caller ตัด TCP ทิ้งระหว่างรอ RPC
         แถว request_logs จะค้างไม่มี http_status
- Task 12: minor (deferred, plan-mandated): forward ทุก header ของ caller เข้า AMQP message
- Task 12: minor (deferred): IsAllowed ไม่รองรับ CIDR รับเฉพาะ IP ตรงตัว
- Task 12: minor (deferred): เช็ค method (405) มาก่อนเช็ค IP (403) — ทำให้คนนอก whitelist
         แยกออกได้ว่า endpoint มีอยู่จริงไหมจาก status ที่ต่างกัน
- Task 12: complete (commits 929c8d7..8213983 บน wt/httpapi, review clean หลัง fix round 1)
- Task 13: minor (deferred): GOTOOLCHAIN=local ตั้งเฉพาะบรรทัด go build ไม่ได้ตั้งตอน go mod download (พ่วงให้แก้)
- Task 13: minor (deferred): cancel() ใน main มีผลสองอย่างพร้อมกัน (หยุด reconcile loop + สั่ง
         watchdog ของทุก flow ยกเลิก consumer) ควรมีคอมเมนต์บอก
- Task 13: fix round 1/5 ส่งกลับ pane 2 พร้อมยกเลิก freeze ของ internal/flow + internal/reconcile เฉพาะ finding 1
- Task 13: minor (deferred): watchdog อาจเรียก Cancel บน broker ที่ Run เพิ่งปิด (ได้ ErrClosed ทิ้ง)
- Task 13: minor (deferred): Drain บน flow ที่ตายเองจะ log warning cancel ทุกครั้ง
- Task 13: minor (deferred): warnIfTimeoutsExceedGrace ยังใช้เกณฑ์เดิม ไม่ได้ปรับตามงบที่แชร์กันแล้ว
- Task 13: minor (deferred): drainBudget == 0 ทำให้ select ของ Drain นอนดีเทอร์มินิสติก
- Task 13: complete (commits 1f7eac3..24aa553 บน wt/gateway, review clean หลัง fix round 1)

