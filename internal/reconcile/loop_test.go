package reconcile

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/celalsahinaltinisik/internal/flow"
	"github.com/celalsahinaltinisik/internal/model"
)

type fakeLoader struct{ groups []model.GroupSpec }

func (f fakeLoader) LoadGroups(context.Context) ([]model.GroupSpec, error) { return f.groups, nil }

func noFlow(model.GroupSpec) (*flow.Flow, error) {
	return nil, errors.New("ไม่ต้องสร้าง flow จริงใน test นี้")
}

// Loop ที่ประกอบมาโดยไม่ตั้ง Logf ต้องไม่ panic ตอนเจอ action แรก
// ครอบทั้งทาง Skip (ชื่อผิด) และทาง start ที่ Factory ล้ม ซึ่งทั้งคู่ log
func TestLoopWithoutLogfDoesNotPanic(t *testing.T) {
	l := &Loop{
		Loader: fakeLoader{groups: []model.GroupSpec{
			spec("g1", "Bad Name", 50, "https://a"),
			spec("g2", "deposit", 50, "https://b"),
		}},
		Registry: flow.NewRegistry(),
		Factory:  noFlow,
	}
	if err := l.Once(context.Background(), context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
}

// Interval ที่ไม่ได้ตั้งต้องไม่ทำให้ time.NewTicker panic — ถ้า loop นี้ตาย
// ไม่มีใครกู้ flow ที่ตายและไม่มีใครรับ group ใหม่เลยทั้งระบบ
func TestLoopRunWithZeroIntervalDoesNotPanic(t *testing.T) {
	l := &Loop{Loader: fakeLoader{}, Registry: flow.NewRegistry(), Factory: noFlow}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Run(ctx, context.Background())
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run ไม่ยอม return หลัง ctx ถูกยกเลิก")
	}
}
