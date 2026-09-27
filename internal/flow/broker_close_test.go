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

// countingBroker นับจำนวนครั้งที่ Close ถูกเรียก
// ใช้พิสูจน์ว่า Flow ปิด channel ของตัวเองครั้งเดียวพอดี — 0 = channel รั่ว, >1 = ปิดซ้ำ
type countingBroker struct {
	mu        sync.Mutex
	msgs      chan amqp.Delivery
	cancelled bool

	closed   atomic.Int64
	consumed atomic.Bool
	failDecl error
}

func newCountingBroker(buf int) *countingBroker {
	return &countingBroker{msgs: make(chan amqp.Delivery, buf)}
}

func (b *countingBroker) DeclareQueue(string) error { return b.failDecl }

func (b *countingBroker) Consume(string, int) (<-chan amqp.Delivery, string, error) {
	b.consumed.Store(true)
	return b.msgs, "tag-1", nil
}

func (b *countingBroker) Cancel(string) error {
	b.mu.Lock()
	if !b.cancelled {
		b.cancelled = true
		close(b.msgs) // broker จริงปิด channel หลัง delivery สุดท้าย
	}
	b.mu.Unlock()
	return nil
}

func (b *countingBroker) Close() error {
	b.closed.Add(1)
	return nil
}

// Flow เป็นเจ้าของ Broker ตัวนั้นผู้เดียว (factory สร้างใหม่ให้ทุก flow)
// ถ้าไม่มีใครปิด ทุกการ restart/stop จะทิ้ง AMQP channel ค้างบน connection ที่ใช้ร่วมกัน
// จนชน channel_max (default 2047) แล้วเปิด flow ใหม่ไม่ได้อีกเลย
func TestRunClosesBrokerExactlyOnceAfterWorkersFinish(t *testing.T) {
	b := newCountingBroker(1)
	release := make(chan struct{})
	started := make(chan struct{})
	ack := &fakeAck{}

	f := New(Options{Spec: testSpec(1), Queue: "q", Broker: b,
		Process: func(_ context.Context, d amqp.Delivery, _ model.GroupSpec) {
			close(started)
			<-release
			_ = d.Ack(false)
		}})

	done := make(chan error, 1)
	go func() { done <- f.Run(context.Background()) }()

	b.msgs <- amqp.Delivery{Acknowledger: ack}
	<-started

	// worker ยังถือข้อความอยู่ — ห้ามปิด broker ตอนนี้
	if got := b.closed.Load(); got != 0 {
		t.Fatalf("Close ถูกเรียก %d ครั้งตอน worker ยังทำงานอยู่, want 0", got)
	}

	close(release)
	_ = b.Cancel("tag-1")

	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := b.closed.Load(); got != 1 {
		t.Fatalf("Close ถูกเรียก %d ครั้ง, want 1 (0 = channel รั่ว, >1 = ปิดซ้ำ)", got)
	}
}

// ทางที่ล้มตั้งแต่เริ่มก็ต้องไม่ทิ้ง channel ค้าง — flow ที่ declare queue ไม่ผ่าน
// ถูก reconciler mark failed แล้วสร้างใหม่ทุกรอบ ถ้ารั่วตรงนี้จะสะสมเร็วที่สุด
func TestRunClosesBrokerWhenStartupFails(t *testing.T) {
	b := newCountingBroker(0)
	b.failDecl = errors.New("406 PRECONDITION_FAILED")

	f := New(Options{Spec: testSpec(1), Queue: "q", Broker: b,
		Process: func(context.Context, amqp.Delivery, model.GroupSpec) {}})

	if err := f.Run(context.Background()); err == nil {
		t.Fatal("declare ล้มต้องคืน error")
	}
	if got := b.closed.Load(); got != 1 {
		t.Fatalf("Close ถูกเรียก %d ครั้ง, want 1 — startup ล้มก็ต้องคืน channel", got)
	}
}

// Run เป็น single-use — การเรียกซ้ำต้องไม่ไปปิด broker อีกรอบ
func TestSecondRunDoesNotCloseBrokerAgain(t *testing.T) {
	b := newCountingBroker(0)
	f := New(Options{Spec: testSpec(1), Queue: "q", Broker: b,
		Process: func(context.Context, amqp.Delivery, model.GroupSpec) {}})

	done := make(chan error, 1)
	go func() { done <- f.Run(context.Background()) }()
	waitFor(t, func() bool { return b.consumed.Load() })
	_ = b.Cancel("tag-1")
	if err := <-done; err != nil {
		t.Fatalf("Run ครั้งแรก: %v", err)
	}

	if err := f.Run(context.Background()); err == nil {
		t.Fatal("Run ครั้งที่สองต้องคืน error")
	}
	if got := b.closed.Load(); got != 1 {
		t.Fatalf("Close ถูกเรียก %d ครั้ง, want 1 — ห้ามปิดซ้ำ", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("รอเงื่อนไขไม่สำเร็จภายใน 2 วินาที")
		}
		time.Sleep(time.Millisecond)
	}
}
