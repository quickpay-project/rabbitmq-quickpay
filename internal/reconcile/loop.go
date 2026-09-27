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
func (l *Loop) Once(ctx context.Context) error {
	desired, err := l.Loader.LoadGroups(ctx)
	if err != nil {
		return err
	}
	for _, a := range Diff(desired, l.Registry.Snapshot()) {
		l.apply(ctx, a)
	}
	return nil
}

func (l *Loop) apply(ctx context.Context, a Action) {
	switch a.Kind {
	case Skip:
		l.logf("⏭  ข้าม group %s: %s", a.Spec.Name, a.Reason)

	case Start:
		l.start(ctx, a.Spec)

	case HotSwap:
		if f, _, ok := l.Registry.Get(a.ID); ok {
			f.UpdateSpec(a.Spec)
			l.Registry.SetState(a.ID, stateFor(a.Spec))
			l.logf("🔄 %s: อัปเดต url/config โดยไม่ restart", a.Spec.Name)
		}

	case Restart:
		l.logf("♻️  %s: restart (%s)", a.Spec.Name, a.Reason)
		l.stop(a.ID)
		l.start(ctx, a.Spec)

	case Stop:
		l.logf("🛑 %s: ปิด (%s)", a.Spec.Name, a.Reason)
		l.stop(a.ID)
	}
}

func (l *Loop) start(ctx context.Context, spec model.GroupSpec) {
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
		if err := f.Run(ctx); err != nil {
			l.logf("❌ %s: flow หยุดพร้อม error: %v", spec.Name, err)
		} else if !f.Draining() {
			l.logf("⏹  %s: flow หยุดเอง จะกู้ในรอบ reconcile ถัดไป", spec.Name)
		}
		if !f.Draining() {
			l.Registry.SetState(spec.ID, flow.StateFailed)
		}
	}()

	l.logf("▶️  %s: ทำงานแล้ว (worker=%d, url=%d)", spec.Name, spec.WorkerCount, len(spec.URLs))
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
func (l *Loop) Run(ctx context.Context) {
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
			if err := l.Once(ctx); err != nil {
				l.logf("⚠️  reconcile ล้มเหลว: %v", err)
			}
		}
	}
}
