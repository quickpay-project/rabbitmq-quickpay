package reconcile

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/celalsahinaltinisik/internal/flow"
	"github.com/celalsahinaltinisik/internal/model"
	amqp "github.com/rabbitmq/amqp091-go"
)

// countingBroker นับ Close ต่อ broker หนึ่งตัว — factory สร้างใหม่ให้ทุก flow
// จึงใช้เทียบได้ว่า flow ที่ถูก stop ปิด channel ของตัวเองครั้งเดียว
// และ flow ที่ยังทำงานอยู่ไม่ถูกปิดไปด้วย
type countingBroker struct {
	mu        sync.Mutex
	msgs      chan amqp.Delivery
	cancelled bool

	closed   atomic.Int64
	cancels  atomic.Int64
	consumed atomic.Bool
}

func (b *countingBroker) DeclareQueue(string) error { return nil }

func (b *countingBroker) Consume(string, int) (<-chan amqp.Delivery, string, error) {
	b.consumed.Store(true)
	return b.msgs, "tag-1", nil
}

func (b *countingBroker) Cancel(string) error {
	b.cancels.Add(1)
	b.mu.Lock()
	if !b.cancelled {
		b.cancelled = true
		close(b.msgs)
	}
	b.mu.Unlock()
	return nil
}

func (b *countingBroker) Close() error {
	b.closed.Add(1)
	return nil
}

// brokerFactory เก็บ broker ทุกตัวที่ถูกสร้าง เรียงตามลำดับการสร้าง
type brokerFactory struct {
	mu      sync.Mutex
	created []*countingBroker
	flows   []*flow.Flow
}

func (bf *brokerFactory) next() *countingBroker {
	b := &countingBroker{msgs: make(chan amqp.Delivery)}
	bf.mu.Lock()
	bf.created = append(bf.created, b)
	bf.mu.Unlock()
	return b
}

func (bf *brokerFactory) at(i int) *countingBroker {
	bf.mu.Lock()
	defer bf.mu.Unlock()
	if i >= len(bf.created) {
		return nil
	}
	return bf.created[i]
}

func (bf *brokerFactory) count() int {
	bf.mu.Lock()
	defer bf.mu.Unlock()
	return len(bf.created)
}

func (bf *brokerFactory) record(f *flow.Flow) *flow.Flow {
	bf.mu.Lock()
	bf.flows = append(bf.flows, f)
	bf.mu.Unlock()
	return f
}

// flowAt คืน *flow.Flow ตัวที่ i ตามลำดับการสร้าง — ใช้เทียบ identity
func (bf *brokerFactory) flowAt(i int) *flow.Flow {
	bf.mu.Lock()
	defer bf.mu.Unlock()
	if i >= len(bf.flows) {
		return nil
	}
	return bf.flows[i]
}

type listLoader struct {
	mu     sync.Mutex
	groups []model.GroupSpec
}

func (l *listLoader) LoadGroups(context.Context) ([]model.GroupSpec, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.groups, nil
}

func (l *listLoader) set(g ...model.GroupSpec) {
	l.mu.Lock()
	l.groups = g
	l.mu.Unlock()
}

func loopWithCountingBrokers(t *testing.T) (*Loop, *listLoader, *brokerFactory) {
	t.Helper()
	bf := &brokerFactory{}
	ld := &listLoader{}
	l := &Loop{
		Loader:   ld,
		Registry: flow.NewRegistry(),
		Factory: func(s model.GroupSpec) (*flow.Flow, error) {
			return bf.record(flow.New(flow.Options{Spec: s, Queue: "v2." + s.Name,
				Broker:  bf.next(),
				Process: func(context.Context, amqp.Delivery, model.GroupSpec) {}})), nil
		},
		Drain: 2 * time.Second,
	}
	return l, ld, bf
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("รอ %s ไม่สำเร็จภายใน 2 วินาที", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// group ที่ถูกลบออกจาก DB → action Stop → flow ต้องคืน AMQP channel ของตัวเอง
func TestStoppedFlowClosesItsBrokerExactlyOnce(t *testing.T) {
	l, ld, bf := loopWithCountingBrokers(t)
	ctx := context.Background()

	ld.set(spec("g1", "withdraw", 2, "https://a"))
	if err := l.Once(ctx, ctx); err != nil {
		t.Fatalf("Once (start): %v", err)
	}
	waitFor(t, "flow เริ่ม consume", func() bool {
		b := bf.at(0)
		return b != nil && b.consumed.Load()
	})
	if got := bf.at(0).closed.Load(); got != 0 {
		t.Fatalf("Close ถูกเรียก %d ครั้งตอน flow ยังทำงาน, want 0", got)
	}

	ld.set() // DB ไม่มี group นี้แล้ว
	if err := l.Once(ctx, ctx); err != nil {
		t.Fatalf("Once (stop): %v", err)
	}

	if got := bf.at(0).closed.Load(); got != 1 {
		t.Fatalf("Close ถูกเรียก %d ครั้ง, want 1 ต่อ flow ที่ถูก stop", got)
	}
	if n := bf.count(); n != 1 {
		t.Errorf("สร้าง broker %d ตัว, want 1", n)
	}
}

// worker_count เปลี่ยน → action Restart → ตัวเก่าต้องถูกปิด ตัวใหม่ต้องไม่ถูกปิด
// นี่คือเส้นทางที่ทำให้รั่วเร็วที่สุด เพราะ config เปลี่ยนได้ตลอดผ่าน DB
func TestRestartClosesOldBrokerButNotNewOne(t *testing.T) {
	l, ld, bf := loopWithCountingBrokers(t)
	ctx := context.Background()

	ld.set(spec("g1", "withdraw", 2, "https://a"))
	if err := l.Once(ctx, ctx); err != nil {
		t.Fatalf("Once (start): %v", err)
	}
	waitFor(t, "flow แรกเริ่ม consume", func() bool {
		b := bf.at(0)
		return b != nil && b.consumed.Load()
	})

	ld.set(spec("g1", "withdraw", 5, "https://a")) // worker_count 2 → 5
	if err := l.Once(ctx, ctx); err != nil {
		t.Fatalf("Once (restart): %v", err)
	}
	waitFor(t, "flow ใหม่เริ่ม consume", func() bool {
		b := bf.at(1)
		return b != nil && b.consumed.Load()
	})

	if n := bf.count(); n != 2 {
		t.Fatalf("สร้าง broker %d ตัว, want 2", n)
	}
	if got := bf.at(0).closed.Load(); got != 1 {
		t.Errorf("broker ตัวเก่า: Close %d ครั้ง, want 1", got)
	}
	if got := bf.at(1).closed.Load(); got != 0 {
		t.Errorf("broker ตัวใหม่: Close %d ครั้ง, want 0 — ยังทำงานอยู่ ห้ามปิด", got)
	}

	// เก็บกวาด flow ที่ยังรันอยู่ ไม่ให้ goroutine ค้างข้าม test
	_ = bf.at(1).Cancel("tag-1")
	waitFor(t, "flow ใหม่ปิด broker หลังถูก cancel", func() bool {
		return bf.at(1).closed.Load() == 1
	})
}
