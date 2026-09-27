package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/celalsahinaltinisik/internal/flow"
	"github.com/celalsahinaltinisik/internal/model"
	amqp "github.com/rabbitmq/amqp091-go"
)

// M1 — ctx ของ reconcile loop ต้องแยกจาก ctx ของ flow
//
// ถ้าใช้ตัวเดียวกัน การยกเลิกตอน SIGTERM จะวิ่งไปถึง Process → Processor.Handle
// แล้วฆ่า payment ที่ค้างกลางทางทุกตัวพร้อมกัน: forward ได้ context.Canceled หลัง
// WroteRequest → Classify คืน fatal (ออเดอร์สถานะไม่แน่นอน), attempt/request log
// เขียนไม่ลง และ reply ส่งไม่ออก — ดีไซน์ drain ทั้งหมดกลายเป็น dead code
func TestCancellingLoopCtxDoesNotKillRunningFlows(t *testing.T) {
	bf := &brokerFactory{}
	ld := &listLoader{}
	seen := make(chan error, 1)

	l := &Loop{
		Loader:   ld,
		Registry: flow.NewRegistry(),
		Factory: func(s model.GroupSpec) (*flow.Flow, error) {
			return bf.record(flow.New(flow.Options{Spec: s, Queue: "v2." + s.Name,
				Broker: bf.next(),
				Process: func(ctx context.Context, _ amqp.Delivery, _ model.GroupSpec) {
					seen <- ctx.Err()
				}})), nil
		},
		Drain: 2 * time.Second,
	}

	loopCtx, loopCancel := context.WithCancel(context.Background())
	flowCtx, flowCancel := context.WithCancel(context.Background())
	defer flowCancel()

	ld.set(spec("g1", "withdraw", 1, "https://a"))
	if err := l.Once(loopCtx, flowCtx); err != nil {
		t.Fatalf("Once: %v", err)
	}
	waitFor(t, "flow เริ่ม consume", func() bool {
		b := bf.at(0)
		return b != nil && b.consumed.Load()
	})

	loopCancel() // เหมือนได้รับ SIGTERM

	// flow ต้องยังรับข้อความต่อ และ ctx ที่ถึง Process ต้องยังใช้งานได้
	select {
	case bf.at(0).msgs <- amqp.Delivery{}:
	case <-time.After(2 * time.Second):
		t.Fatal("flow ไม่รับข้อความอีกหลังยกเลิก loop ctx")
	}
	select {
	case err := <-seen:
		if err != nil {
			t.Fatalf("ctx ที่ส่งถึง Process = %v, want nil — SIGTERM ต้องไม่ฆ่างานที่ค้างอยู่", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Process ไม่ถูกเรียกหลังยกเลิก loop ctx")
	}

	// และห้ามมีใครสั่ง Cancel consumer เพียงเพราะ loop ctx ตาย
	if got := bf.at(0).cancels.Load(); got != 0 {
		t.Errorf("Broker.Cancel ถูกเรียก %d ครั้งหลังยกเลิก loop ctx, want 0 — "+
			"การหยุดรับงานใหม่ต้องมาจาก Drain เท่านั้น", got)
	}

	// เก็บกวาด
	_ = bf.at(0).Cancel("tag-1")
	waitFor(t, "flow ปิด broker หลังถูก cancel", func() bool { return bf.at(0).closed.Load() == 1 })
}
