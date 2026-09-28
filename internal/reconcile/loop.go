package reconcile

import (
	"context"
	"time"

	"github.com/celalsahinaltinisik/internal/flow"
	"github.com/celalsahinaltinisik/internal/model"
)

type GroupLoader interface {
	LoadGroups(ctx context.Context) ([]model.GroupSpec, error)
}

// FlowFactory สร้าง Flow ตัวใหม่จาก spec — cmd/gateway เป็นคนใส่ของจริงเข้ามา
type FlowFactory func(spec model.GroupSpec) (*flow.Flow, error)

type Loop struct {
	Loader   GroupLoader
	Registry *flow.Registry
	Factory  FlowFactory
	Interval time.Duration
	Drain    time.Duration
	Logf     func(string, ...any)
}

// defaultInterval ใช้เมื่อ Interval ไม่ได้ตั้งมา — ตรงกับ default ของ RECONCILE_INTERVAL ใน config
const defaultInterval = 30 * time.Second

// logf กัน Loop ที่ประกอบมาไม่ครบไม่ให้ panic เหมือนที่ flow.New default Logf เป็น no-op
// ถ้า reconcile loop panic ระบบ dynamic ตายยกชุด: ไม่มีใครกู้ flow ที่ตาย ไม่มีใครรับ group ใหม่
// การเงียบไปหนึ่งบรรทัด log แลกกับการที่ loop ยังเดินต่อได้ เป็นการแลกที่คุ้มกว่ามาก
func (l *Loop) logf(format string, args ...any) {
	if l.Logf == nil {
		return
	}
	l.Logf(format, args...)
}

// Once ทำ reconcile หนึ่งรอบ ใช้ทั้งตอน start (แบบ sync) และในลูป
//
// ctx กับ flowCtx ต้องเป็น "คนละตัว" และห้ามรวมกลับเป็นตัวเดียวเด็ดขาด
//   - ctx     คุมงานควบคุม: อ่าน DB รอบนี้ ยกเลิกได้ทันทีตอน shutdown
//   - flowCtx คุมอายุของ flow ที่ถูกสร้าง ซึ่งถูกส่งต่อไปถึง Process และ request
//     ที่ worker ถืออยู่ ถ้าเอา ctx ตัวเดียวกันมาใช้ การยกเลิกตอน SIGTERM
//     จะฆ่า payment ที่ค้างกลางทางทุกตัวพร้อมกัน แทนที่จะ drain ให้จบ
//     การหยุดรับงานใหม่เป็นหน้าที่ของ Flow.Drain (Broker.Cancel) ไม่ใช่ ctx cancel
func (l *Loop) Once(ctx, flowCtx context.Context) error {
	desired, err := l.Loader.LoadGroups(ctx)
	if err != nil {
		return err
	}

	// DB ที่ไม่มี group เลยคือภาวะที่ service ดูปกติทุกอย่างแต่ตายสนิท:
	// สตาร์ทสำเร็จ, /readyz ตอบ ready:true (registry ว่าง = ไม่มีใครไม่พร้อม)
	// แต่ทุก request ได้ 404 เพราะไม่มี flow ให้ route ไปหา
	// เกิดได้จริงตอนขึ้น prod เพราะ migration สร้างแต่ตาราง ไม่ได้ใส่ group ให้
	//
	// เตือนทุกรอบไม่ใช่ครั้งเดียวตอน start โดยตั้งใจ — คำเตือนตอนบูตจะเลื่อนหายไปจากจอ
	// ก่อนที่ใครจะมาดู และนี่คือภาวะที่ควรดังจนกว่าจะมีคนแก้ ไม่ใช่เหตุการณ์ที่ผ่านไปแล้ว
	// ไม่ทำให้ readiness แดงเพราะ restart ไม่ได้ทำให้ group โผล่ขึ้นมา และการรันโดยยังไม่มี
	// group เป็นเรื่องปกติของ dev — ความจริงต้องดัง แต่ไม่ควรกลายเป็น pod ที่ไม่มีวันพร้อม
	if len(desired) == 0 {
		l.logf("⚠️  ไม่มี group ใน DB เลย — ทุก request จะได้ 404 ทั้งที่ /readyz ยังเขียว " +
			"(รัน ./scripts/seed-groups.sh --apply หรือเพิ่มแถวใน message_group)")
	}

	for _, a := range Diff(desired, l.Registry.Snapshot()) {
		l.apply(flowCtx, a)
	}
	return nil
}

// apply รับ flowCtx เพราะ action เดียวที่ต้องใช้ ctx คือ start ซึ่งเป็นคนกำหนดอายุของ flow
func (l *Loop) apply(flowCtx context.Context, a Action) {
	switch a.Kind {
	case Skip:
		l.logf("⏭  ข้าม group %s: %s", a.Spec.Name, a.Reason)

	case Start:
		l.start(flowCtx, a.Spec)

	case HotSwap:
		if f, _, ok := l.Registry.Get(a.ID); ok {
			f.UpdateSpec(a.Spec)
			l.Registry.SetState(a.ID, stateFor(a.Spec))
			l.logf("🔄 %s: อัปเดต url/config โดยไม่ restart", a.Spec.Name)
		}

	case Restart:
		l.logf("♻️  %s: restart (%s)", a.Spec.Name, a.Reason)
		l.stop(a.ID)
		l.start(flowCtx, a.Spec)

	case Stop:
		l.logf("🛑 %s: ปิด (%s)", a.Spec.Name, a.Reason)
		l.stop(a.ID)
	}
}

// start สร้างแล้วสตาร์ท flow — flowCtx เป็นอายุของ flow ตัวนั้น ไม่ใช่ของ reconcile loop
func (l *Loop) start(flowCtx context.Context, spec model.GroupSpec) {
	f, err := l.Factory(spec)
	if err != nil {
		l.logf("❌ %s: สร้าง flow ไม่สำเร็จ: %v", spec.Name, err)
		return
	}
	// ต้อง Put ด้วยสถานะสุดท้ายก่อนสตาร์ท goroutine
	// ถ้า Put เป็น starting แล้วค่อย SetState ทีหลัง goroutine ที่ Run ล้มทันที
	// (เช่น declare queue เจอ 406) จะ set failed ก่อน แล้วโดนเขียนทับเป็น running
	// ทำให้ reconciler ไม่รู้ว่า flow ตายและไม่กู้ให้
	l.Registry.Put(spec.ID, f, stateFor(spec))

	go func() {
		// Run คืนค่าเมื่อ channel ปิด — ถือเป็นการตายที่ต้องกู้ในรอบถัดไป
		if err := f.Run(flowCtx); err != nil {
			l.logf("❌ %s: flow หยุดพร้อม error: %v", spec.Name, err)
		} else if !f.Draining() {
			l.logf("⏹  %s: flow หยุดเอง จะกู้ในรอบ reconcile ถัดไป", spec.Name)
		}
		if !f.Draining() {
			// goroutine ถูก deschedule ตรงนี้ได้ และระหว่างนั้น Restart ทั้งชุด
			// (stop + Remove + start + Put ตัวใหม่) อาจเสร็จไปแล้ว การ mark จึงต้อง
			// เช็ค identity ไม่ใช่เช็คแค่ว่า id ยังมีอยู่
			l.markFailedIfStillCurrent(spec.ID, f, spec.Name)
		}
	}()

	l.logf("▶️  %s: ทำงานแล้ว (worker=%d, url=%d)", spec.Name, spec.WorkerCount, len(spec.URLs))
}

// markFailedIfStillCurrent mark flow ว่า failed เพื่อให้รอบ reconcile ถัดไปกู้ให้
// แต่ต้องเป็น flow ตัวปัจจุบันของ id นั้นจริง ๆ เท่านั้น
//
// ผู้เรียกเช็ค Draining มาแล้ว เมธอดนี้รับผิดชอบเฉพาะการเช็ค identity — แยกกันเพื่อให้
// เทสต์เรียกได้ในสถานะเดียวกับ goroutine ที่ผ่านการเช็ค Draining ไปแล้วแต่ยังไม่ได้ mark
//
// ตอน Restart มี flow สองตัวใช้ id เดียวกันอยู่ช่วงสั้น ๆ (stop ตัวเก่า → start ตัวใหม่)
// goroutine ของตัวเก่าอาจเพิ่งผ่านการเช็ค Draining แล้วถูก deschedule พอดี ระหว่างนั้น
// stop + Remove + start + Put(ตัวใหม่, Running) เสร็จไปก่อน ถ้าใช้ SetState ธรรมดา
// (ซึ่งเช็คแค่ว่า id มีอยู่) มันจะไป mark flow ตัวใหม่เอี่ยมเป็น failed
// → เกิด Restart ปลอมทุกรอบเวลา config เปลี่ยนบ่อย ซึ่งแต่ละครั้งคือการ drain
// consumer ที่แข็งแรงดีทิ้ง
//
// แยกออกมาเป็นเมธอดเพื่อให้ทดสอบ invariant นี้ได้ตรง ๆ โดยไม่ต้องไปบังคับจังหวะ
// deschedule ของ goroutine ซึ่งบังคับในเทสต์ไม่ได้
func (l *Loop) markFailedIfStillCurrent(id string, f *flow.Flow, name string) {
	if !l.Registry.SetStateIf(id, f, flow.StateFailed) {
		l.logf("🔁 %s: flow ตัวเก่าจบหลังถูกแทนที่แล้ว ไม่แตะสถานะของตัวใหม่", name)
	}
}

func (l *Loop) stop(id string) {
	f, _, ok := l.Registry.Get(id)
	if !ok {
		return
	}
	l.Registry.SetState(id, flow.StateDraining)
	if err := f.Drain(l.Drain); err != nil {
		l.logf("⚠️  drain ไม่จบในเวลา: %v", err)
	}
	l.Registry.Remove(id)
}

func stateFor(s model.GroupSpec) flow.State {
	if !s.HasUpstream() {
		return flow.StateDegraded
	}
	return flow.StateRunning
}

// Run วน reconcile จนกว่า ctx จะถูกยกเลิก
//
// ctx หยุดตัว loop เอง ส่วน flowCtx ส่งต่อให้ flow ที่ถูกสร้างในแต่ละรอบ
// ยกเลิก ctx ตอน shutdown ได้ทันทีโดยไม่กระทบงานที่ worker ถืออยู่
func (l *Loop) Run(ctx, flowCtx context.Context) {
	// time.NewTicker panic ถ้า d <= 0 — ห้ามให้ loop ตายตอนคลอดเพราะ config ที่ประกอบมาไม่ครบ
	interval := l.Interval
	if interval <= 0 {
		l.logf("⚠️  reconcile interval = %v ใช้ไม่ได้ ใช้ค่า default %v แทน", l.Interval, defaultInterval)
		interval = defaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := l.Once(ctx, flowCtx); err != nil {
				l.logf("⚠️  reconcile ล้มเหลว: %v", err)
			}
		}
	}
}
