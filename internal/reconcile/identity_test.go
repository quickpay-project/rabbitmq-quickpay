package reconcile

import (
	"context"
	"testing"

	"github.com/celalsahinaltinisik/internal/flow"
)

// M4 — flow ที่ตายเองต้องถูก mark failed ตามปกติ (ทางกู้ตัวเองต้องไม่พังเพราะ identity check)
// เดินผ่าน goroutine จริงใน Loop.start ไม่ได้เรียก SetStateIf ตรง ๆ
func TestFlowThatDiesOnItsOwnIsMarkedFailed(t *testing.T) {
	l, ld, bf := loopWithCountingBrokers(t)
	ctx := context.Background()

	ld.set(spec("g1", "withdraw", 2, "https://a"))
	if err := l.Once(ctx, ctx); err != nil {
		t.Fatalf("Once: %v", err)
	}
	waitFor(t, "flow เริ่ม consume", func() bool {
		b := bf.at(0)
		return b != nil && b.consumed.Load()
	})

	// จำลอง broker ปิด consumer เอง (connection หลุด) โดยที่ไม่มีใครเรียก Drain
	// → Draining() ยังเป็น false → goroutine ต้อง mark failed
	_ = bf.at(0).Cancel("tag-1")

	waitFor(t, "สถานะกลายเป็น failed", func() bool {
		_, st, ok := l.Registry.Get("g1")
		return ok && st == flow.StateFailed
	})
}

// M4 — หัวใจของ finding: goroutine ของ flow ตัวเก่าที่จบทีหลัง ต้องไม่ mark ตัวใหม่เป็น failed
// ทดสอบที่รอยต่อเดียวกับที่ goroutine เรียกจริง (Registry.SetStateIf ด้วย pointer ตัวเก่า)
// เพราะจังหวะ deschedule ของ goroutine บังคับให้เกิดตรง ๆ ในเทสต์ไม่ได้
func TestStaleFlowGoroutineCannotFailTheReplacement(t *testing.T) {
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

	ld.set(spec("g1", "withdraw", 5, "https://a")) // worker_count เปลี่ยน → Restart
	if err := l.Once(ctx, ctx); err != nil {
		t.Fatalf("Once (restart): %v", err)
	}
	waitFor(t, "flow ใหม่เริ่ม consume", func() bool {
		b := bf.at(1)
		return b != nil && b.consumed.Load()
	})

	oldFlow, newFlow := bf.flowAt(0), bf.flowAt(1)
	if oldFlow == nil || newFlow == nil || oldFlow == newFlow {
		t.Fatalf("ต้องมี flow สองตัวที่ต่างกัน: old=%p new=%p", oldFlow, newFlow)
	}
	if cur, _, _ := l.Registry.Get("g1"); cur != newFlow {
		t.Fatalf("registry ต้องถือ flow ตัวใหม่อยู่")
	}

	// เรียกสิ่งเดียวกับที่ goroutine ของ flow ตัวเก่าเรียกตอนตื่นขึ้นมาทีหลัง
	l.markFailedIfStillCurrent("g1", oldFlow, "withdraw")
	if _, st, _ := l.Registry.Get("g1"); st != flow.StateRunning {
		t.Fatalf("สถานะ = %q, want running — ตัวใหม่ถูก mark failed ทั้งที่แข็งแรงดี "+
			"จะเกิด Restart ปลอมแล้ว drain consumer ที่ใช้งานได้ทิ้ง", st)
	}

	_ = bf.at(1).Cancel("tag-1") // เก็บกวาด
}
