package flow

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/celalsahinaltinisik/internal/model"
	amqp "github.com/rabbitmq/amqp091-go"
)

type fakeAck struct{ acked atomic.Int64 }

func (f *fakeAck) Ack(tag uint64, multiple bool) error              { f.acked.Add(1); return nil }
func (f *fakeAck) Nack(tag uint64, multiple, requeue bool) error    { return nil }
func (f *fakeAck) Reject(tag uint64, requeue bool) error            { return nil }

type fakeBroker struct {
	mu        sync.Mutex
	msgs      chan amqp.Delivery
	declared  []string
	prefetch  int
	cancelled bool
	failDecl  error
}

func newFakeBroker(buf int) *fakeBroker {
	return &fakeBroker{msgs: make(chan amqp.Delivery, buf)}
}

func (b *fakeBroker) DeclareQueue(name string) error {
	if b.failDecl != nil {
		return b.failDecl
	}
	b.mu.Lock()
	b.declared = append(b.declared, name)
	b.mu.Unlock()
	return nil
}

func (b *fakeBroker) Consume(queue string, prefetch int) (<-chan amqp.Delivery, string, error) {
	b.mu.Lock()
	b.prefetch = prefetch
	b.mu.Unlock()
	return b.msgs, "tag-1", nil
}

func (b *fakeBroker) Cancel(tag string) error {
	b.mu.Lock()
	if !b.cancelled {
		b.cancelled = true
		close(b.msgs) // broker จริงจะปิด channel หลัง delivery สุดท้าย
	}
	b.mu.Unlock()
	return nil
}

func (b *fakeBroker) Close() error { return nil }

func testSpec(workers int) model.GroupSpec {
	return model.GroupSpec{ID: "g1", Name: "withdraw", WorkerCount: workers,
		URLs: []model.URLSpec{{ID: 1, URL: "https://a"}}}
}

func TestRunDeclaresQueueAndSetsPrefetchToWorkerCount(t *testing.T) {
	b := newFakeBroker(0)
	f := New(Options{Spec: testSpec(7), Queue: "v2.withdraw", Broker: b,
		Process: func(context.Context, amqp.Delivery, model.GroupSpec) {}})

	done := make(chan error, 1)
	go func() { done <- f.Run(context.Background()) }()
	time.Sleep(50 * time.Millisecond)
	_ = b.Cancel("tag-1")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run ไม่ยอม return")
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.declared) != 1 || b.declared[0] != "v2.withdraw" {
		t.Errorf("declared = %v, want [v2.withdraw]", b.declared)
	}
	if b.prefetch != 7 {
		t.Errorf("prefetch = %d, want 7 — ต้องเท่ากับ worker_count ไม่งั้น worker ส่วนใหญ่จะว่าง", b.prefetch)
	}
}

// บทเรียนจาก conswithdraw.go:288 ที่ใช้ select {} แล้วกู้ตัวเองไม่ได้
func TestRunReturnsWhenDeliveryChannelCloses(t *testing.T) {
	b := newFakeBroker(0)
	f := New(Options{Spec: testSpec(3), Queue: "q", Broker: b,
		Process: func(context.Context, amqp.Delivery, model.GroupSpec) {}})

	done := make(chan error, 1)
	go func() { done <- f.Run(context.Background()) }()
	time.Sleep(20 * time.Millisecond)
	_ = b.Cancel("tag-1")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run คืน error %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run ต้อง return เมื่อ channel ปิด ห้าม block ถาวร")
	}
}

func TestRunReturnsErrorWhenDeclareFails(t *testing.T) {
	b := newFakeBroker(0)
	b.failDecl = errors.New("406 PRECONDITION_FAILED")
	f := New(Options{Spec: testSpec(1), Queue: "q", Broker: b,
		Process: func(context.Context, amqp.Delivery, model.GroupSpec) {}})

	if err := f.Run(context.Background()); err == nil {
		t.Fatal("declare ล้มต้องคืน error เพื่อให้ reconciler mark failed แล้วลองใหม่")
	}
}

func TestWorkersProcessEveryDelivery(t *testing.T) {
	b := newFakeBroker(10)
	var handled atomic.Int64
	ack := &fakeAck{}

	f := New(Options{Spec: testSpec(4), Queue: "q", Broker: b,
		Process: func(_ context.Context, d amqp.Delivery, _ model.GroupSpec) {
			handled.Add(1)
			_ = d.Ack(false)
		}})

	for i := 0; i < 10; i++ {
		b.msgs <- amqp.Delivery{Acknowledger: ack, Body: []byte("x")}
	}

	done := make(chan error, 1)
	go func() { done <- f.Run(context.Background()) }()
	time.Sleep(100 * time.Millisecond)
	_ = b.Cancel("tag-1")
	<-done

	if handled.Load() != 10 {
		t.Fatalf("ประมวลผล %d ข้อความ, want 10", handled.Load())
	}
	if ack.acked.Load() != 10 {
		t.Fatalf("ack %d ครั้ง, want 10", ack.acked.Load())
	}
}

func TestUpdateSpecIsVisibleToWorkers(t *testing.T) {
	b := newFakeBroker(2)
	seen := make(chan string, 2)
	ack := &fakeAck{}

	f := New(Options{Spec: testSpec(1), Queue: "q", Broker: b,
		Process: func(_ context.Context, d amqp.Delivery, s model.GroupSpec) {
			seen <- s.URLs[0].URL
			_ = d.Ack(false)
		}})

	done := make(chan error, 1)
	go func() { done <- f.Run(context.Background()) }()

	b.msgs <- amqp.Delivery{Acknowledger: ack}
	if got := <-seen; got != "https://a" {
		t.Fatalf("url แรก = %q", got)
	}

	next := testSpec(1)
	next.URLs = []model.URLSpec{{ID: 2, URL: "https://b"}}
	f.UpdateSpec(next)

	b.msgs <- amqp.Delivery{Acknowledger: ack}
	if got := <-seen; got != "https://b" {
		t.Fatalf("url หลัง UpdateSpec = %q, want https://b — hot-swap ต้องมีผลโดยไม่ restart", got)
	}

	_ = b.Cancel("tag-1")
	<-done
}

func TestDrainCancelsConsumerAndWaits(t *testing.T) {
	b := newFakeBroker(0)
	f := New(Options{Spec: testSpec(2), Queue: "q", Broker: b,
		Process: func(context.Context, amqp.Delivery, model.GroupSpec) {}})

	go func() { _ = f.Run(context.Background()) }()
	time.Sleep(30 * time.Millisecond)

	if err := f.Drain(2 * time.Second); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.cancelled {
		t.Fatal("Drain ต้องเรียก Cancel ก่อน ไม่งั้น consumer จะดูดงานใหม่ระหว่าง shutdown")
	}
}

func TestDrainTimesOutWhenWorkerStuck(t *testing.T) {
	b := newFakeBroker(1)
	release := make(chan struct{})
	ack := &fakeAck{}

	f := New(Options{Spec: testSpec(1), Queue: "q", Broker: b,
		Process: func(context.Context, amqp.Delivery, model.GroupSpec) { <-release }})

	go func() { _ = f.Run(context.Background()) }()
	b.msgs <- amqp.Delivery{Acknowledger: ack}
	time.Sleep(30 * time.Millisecond)

	err := f.Drain(100 * time.Millisecond)
	if err == nil {
		t.Fatal("worker ที่ค้างเกิน deadline ต้องทำให้ Drain คืน error เพื่อให้ log เห็น")
	}
	close(release)
}
