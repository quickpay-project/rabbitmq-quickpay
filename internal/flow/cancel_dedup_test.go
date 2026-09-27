package flow

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/celalsahinaltinisik/internal/model"
	amqp "github.com/rabbitmq/amqp091-go"
)

// strictBroker นับจำนวนครั้งที่ Cancel ถูกเรียก "จริง" และไม่ทำ de-dup ให้เอง
//
// fake ตัวอื่นในแพ็กเกจนี้ (fakeBroker, countingBroker) guard ด้วย `if !cancelled`
// ซึ่งกลืนการเรียกซ้ำไปเงียบ ๆ ทำให้ test มองไม่เห็นว่า Flow เรียกซ้ำหรือไม่
// ของจริงคือ amqp091: Cancel ซ้ำบน tag เดิมได้ 404 ที่ทำลาย channel ทิ้ง "ระหว่าง drain"
// แล้วข้อความที่ยังไม่ ack จะถูก requeue ซึ่งในระบบนี้แปลว่าออเดอร์ซ้ำ
// ตัวนับคือสิ่งที่ assert — การปิด channel ยัง guard ด้วย sync.Once เพียงเพื่อไม่ให้
// test panic ตัวเองก่อนจะได้อ่านค่าตัวนับ
type strictBroker struct {
	msgs chan amqp.Delivery

	cancels  atomic.Int64
	closes   atomic.Int64
	consumed atomic.Bool
	once     sync.Once
}

func newStrictBroker() *strictBroker {
	return &strictBroker{msgs: make(chan amqp.Delivery)}
}

func (b *strictBroker) DeclareQueue(string) error { return nil }

func (b *strictBroker) Consume(string, int) (<-chan amqp.Delivery, string, error) {
	b.consumed.Store(true)
	return b.msgs, "tag-1", nil
}

func (b *strictBroker) Cancel(string) error {
	b.cancels.Add(1)
	b.once.Do(func() { close(b.msgs) })
	return nil
}

func (b *strictBroker) Close() error {
	b.closes.Add(1)
	return nil
}

func startStrictFlow(t *testing.T, ctx context.Context) (*Flow, *strictBroker, chan error) {
	t.Helper()
	b := newStrictBroker()
	f := New(Options{Spec: testSpec(2), Queue: "q", Broker: b,
		Process: func(context.Context, amqp.Delivery, model.GroupSpec) {}})

	runDone := make(chan error, 1)
	go func() { runDone <- f.Run(ctx) }()
	waitFor(t, func() bool { return b.consumed.Load() })
	return f, b, runDone
}

// M3(a) — Drain กับการยกเลิก ctx แข่งกัน: Broker.Cancel ต้องถูกยิงครั้งเดียวพอดี
// de-dup ตัวนี้เคยถูกทำพังแล้วซ่อมกลับโดยไม่มี test ยืนยัน และการแก้ M1 ก็ไปแตะ
// ctx wiring จุดเดียวกันนี้ จึงต้องมีตัวคุมไว้ถาวร
func TestCancelIsSentOnceWhenDrainRacesCtxCancel(t *testing.T) {
	const rounds = 100
	for i := 0; i < rounds; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		f, b, runDone := startStrictFlow(t, ctx)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); cancel() }()
		go func() { defer wg.Done(); _ = f.Drain(2 * time.Second) }()
		wg.Wait()

		select {
		case err := <-runDone:
			if err != nil {
				t.Fatalf("รอบ %d: Run คืน error %v", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("รอบ %d: Run ไม่ยอม return", i)
		}

		if got := b.cancels.Load(); got != 1 {
			t.Fatalf("รอบ %d: Broker.Cancel ถูกเรียก %d ครั้ง, want 1 — "+
				"การเรียกซ้ำบน tag เดิมได้ 404 ที่ทำลาย channel กลาง drain "+
				"แล้วข้อความที่ยังไม่ ack จะถูก requeue = ออเดอร์ซ้ำ", i, got)
		}
		if got := b.closes.Load(); got != 1 {
			t.Fatalf("รอบ %d: Broker.Close ถูกเรียก %d ครั้ง, want 1", i, got)
		}
		cancel()
	}
}

// M3(b) — ยกเลิก ctx ของ Flow.Run ล้วน ๆ: watchdog ที่ flow.go ต้องหยุดรับงานใหม่
// แล้ว Run คืนค่า — เส้นทางนี้เดิมมี coverage ศูนย์ ไม่มี test ไหนใน repo cancel ctx ของ Flow เลย
func TestCtxCancelStopsFlowWithExactlyOneCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f, b, runDone := startStrictFlow(t, ctx)
	_ = f

	cancel()

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run คืน error %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ยกเลิก ctx แล้ว Run ต้อง return — watchdog ต้องสั่ง Cancel ให้ msgs ปิด")
	}

	if got := b.cancels.Load(); got != 1 {
		t.Errorf("Broker.Cancel ถูกเรียก %d ครั้ง, want 1", got)
	}
	if got := b.closes.Load(); got != 1 {
		t.Errorf("Broker.Close ถูกเรียก %d ครั้ง, want 1 — channel ต้องถูกคืน", got)
	}
}
