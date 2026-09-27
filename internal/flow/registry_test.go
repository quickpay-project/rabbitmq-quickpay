package flow

import (
	"context"
	"testing"

	"github.com/celalsahinaltinisik/internal/model"
	amqp "github.com/rabbitmq/amqp091-go"
)

func TestRegistryPutAndLookupByName(t *testing.T) {
	r := NewRegistry()
	f := New(Options{Spec: testSpec(1), Queue: "q", Broker: newFakeBroker(0),
		Process: func(context.Context, amqp.Delivery, model.GroupSpec) {}})

	r.Put("g1", f, StateRunning)

	got, st, ok := r.ByName("withdraw")
	if !ok || got != f || st != StateRunning {
		t.Fatalf("ByName = %v %v %v", got, st, ok)
	}
	if _, _, ok := r.ByName("ไม่มี"); ok {
		t.Error("ชื่อที่ไม่มีต้องคืน false")
	}
}

func TestRegistrySnapshotCarriesRevision(t *testing.T) {
	r := NewRegistry()
	spec := testSpec(3)
	f := New(Options{Spec: spec, Queue: "q", Broker: newFakeBroker(0),
		Process: func(context.Context, amqp.Delivery, model.GroupSpec) {}})
	r.Put("g1", f, StateRunning)

	snap := r.Snapshot()
	e, ok := snap["g1"]
	if !ok {
		t.Fatal("ไม่พบ g1 ใน snapshot")
	}
	if e.Revision != spec.Revision() {
		t.Error("Revision ใน snapshot ต้องมาจาก spec ปัจจุบันของ flow")
	}
	if e.WorkerCount != 3 || e.Name != "withdraw" {
		t.Errorf("entry = %+v", e)
	}
}

func TestRegistryAllRunningReportsPerFlow(t *testing.T) {
	r := NewRegistry()
	mk := func(name string) *Flow {
		s := testSpec(1)
		s.Name = name
		return New(Options{Spec: s, Queue: "q", Broker: newFakeBroker(0),
			Process: func(context.Context, amqp.Delivery, model.GroupSpec) {}})
	}
	r.Put("g1", mk("withdraw"), StateRunning)
	r.Put("g2", mk("deposit"), StateFailed)

	ok, states := r.AllRunning()
	if ok {
		t.Error("มี flow ที่ failed อยู่ ต้องไม่ ready")
	}
	if states["deposit"] != StateFailed {
		t.Errorf("states = %v", states)
	}
}

func TestRegistryRemove(t *testing.T) {
	r := NewRegistry()
	f := New(Options{Spec: testSpec(1), Queue: "q", Broker: newFakeBroker(0),
		Process: func(context.Context, amqp.Delivery, model.GroupSpec) {}})
	r.Put("g1", f, StateRunning)
	r.Remove("g1")
	if _, _, ok := r.ByName("withdraw"); ok {
		t.Fatal("ลบแล้วต้องหาไม่เจอ")
	}
	if len(r.Snapshot()) != 0 {
		t.Fatal("snapshot ต้องว่าง")
	}
}
