package flow

import (
	"context"
	"testing"

	"github.com/celalsahinaltinisik/internal/model"
	amqp "github.com/rabbitmq/amqp091-go"
)

func noopFlow() *Flow {
	return New(Options{Spec: testSpec(1), Queue: "q", Broker: newFakeBroker(0),
		Process: func(context.Context, amqp.Delivery, model.GroupSpec) {}})
}

// M4 — goroutine ของ flow ตัวเก่าต้องเปลี่ยนสถานะของ flow ตัวใหม่ไม่ได้
func TestSetStateIfIgnoresStaleFlow(t *testing.T) {
	r := NewRegistry()
	old, cur := noopFlow(), noopFlow()

	r.Put("g1", old, StateRunning)
	r.Remove("g1")
	r.Put("g1", cur, StateRunning) // แทนที่ด้วยตัวใหม่ (เส้นทาง Restart)

	if r.SetStateIf("g1", old, StateFailed) {
		t.Error("SetStateIf ต้องคืน false เมื่อ f ไม่ใช่ตัวปัจจุบันแล้ว")
	}
	if _, st, _ := r.Get("g1"); st != StateRunning {
		t.Fatalf("สถานะ = %q, want running — flow ตัวเก่าไป mark ตัวใหม่เป็น failed ไม่ได้", st)
	}
}

func TestSetStateIfUpdatesCurrentFlow(t *testing.T) {
	r := NewRegistry()
	f := noopFlow()
	r.Put("g1", f, StateRunning)

	if !r.SetStateIf("g1", f, StateFailed) {
		t.Fatal("SetStateIf ต้องคืน true เมื่อเป็น flow ตัวปัจจุบัน — ไม่งั้น flow ที่ตายจริงจะไม่ถูกกู้")
	}
	if _, st, _ := r.Get("g1"); st != StateFailed {
		t.Fatalf("สถานะ = %q, want failed", st)
	}
}

func TestSetStateIfOnUnknownIDDoesNothing(t *testing.T) {
	r := NewRegistry()
	if r.SetStateIf("ไม่มี", noopFlow(), StateFailed) {
		t.Error("id ที่ไม่มีอยู่ต้องคืน false")
	}
}
