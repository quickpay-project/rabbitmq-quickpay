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
		l.Logf("⏭  ข้าม group %s: %s", a.Spec.Name, a.Reason)

	case Start:
		l.start(ctx, a.Spec)

	case HotSwap:
		if f, _, ok := l.Registry.Get(a.ID); ok {
			f.UpdateSpec(a.Spec)
			l.Registry.SetState(a.ID, stateFor(a.Spec))
			l.Logf("🔄 %s: อัปเดต url/config โดยไม่ restart", a.Spec.Name)
		}

	case Restart:
		l.Logf("♻️  %s: restart (%s)", a.Spec.Name, a.Reason)
		l.stop(a.ID)
		l.start(ctx, a.Spec)

	case Stop:
		l.Logf("🛑 %s: ปิด (%s)", a.Spec.Name, a.Reason)
		l.stop(a.ID)
	}
}

func (l *Loop) start(ctx context.Context, spec model.GroupSpec) {
	f, err := l.Factory(spec)
	if err != nil {
		l.Logf("❌ %s: สร้าง flow ไม่สำเร็จ: %v", spec.Name, err)
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
			l.Logf("❌ %s: flow หยุดพร้อม error: %v", spec.Name, err)
		} else if !f.Draining() {
			l.Logf("⏹  %s: flow หยุดเอง จะกู้ในรอบ reconcile ถัดไป", spec.Name)
		}
		if !f.Draining() {
			l.Registry.SetState(spec.ID, flow.StateFailed)
		}
	}()

	l.Logf("▶️  %s: ทำงานแล้ว (worker=%d, url=%d)", spec.Name, spec.WorkerCount, len(spec.URLs))
}

func (l *Loop) stop(id string) {
	f, _, ok := l.Registry.Get(id)
	if !ok {
		return
	}
	l.Registry.SetState(id, flow.StateDraining)
	if err := f.Drain(l.Drain); err != nil {
		l.Logf("⚠️  drain ไม่จบในเวลา: %v", err)
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
	t := time.NewTicker(l.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := l.Once(ctx); err != nil {
				l.Logf("⚠️  reconcile ล้มเหลว: %v", err)
			}
		}
	}
}
