package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func drain(t *testing.T, b *BlockedBuffer, want int, got *[]BlockedInput, mu *sync.Mutex) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(*got)
		mu.Unlock()
		if n >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("รอ %d รายการไม่ครบภายในเวลา", want)
}

func TestBlockedBufferWritesThrough(t *testing.T) {
	var mu sync.Mutex
	var seen []BlockedInput
	b := NewBlockedBuffer(func(_ context.Context, in BlockedInput) error {
		mu.Lock()
		seen = append(seen, in)
		mu.Unlock()
		return nil
	}, 8)

	ctx, cancel := context.WithCancel(context.Background())
	go b.Run(ctx)
	defer cancel()

	b.Record(BlockedInput{ClientIP: "1.2.3.4", Path: "/deposit", Reason: BlockedNotInAllowlist})
	drain(t, b, 1, &seen, &mu)
	if seen[0].ClientIP != "1.2.3.4" || seen[0].Reason != BlockedNotInAllowlist {
		t.Fatalf("ได้ %+v", seen[0])
	}
}

// Record อยู่ในเส้นทางของทุก request ที่ถูกปฏิเสธ ห้ามบล็อกแม้คิวเต็มและไม่มีใครอ่าน
// ถ้าบล็อก การยิงถล่มจะทำให้ goroutine ของ HTTP ค้างสะสมจนตาย
func TestBlockedBufferNeverBlocksWhenFull(t *testing.T) {
	b := NewBlockedBuffer(func(context.Context, BlockedInput) error { return nil }, 2)
	// ไม่เรียก Run เลย ไม่มีใครดึงออก

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			b.Record(BlockedInput{ClientIP: "9.9.9.9", Path: "/x", Reason: BlockedNotInAllowlist})
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record บล็อกตอนคิวเต็ม")
	}
	if b.Dropped() != 998 {
		t.Fatalf("อยากได้ทิ้ง 998 (คิวรับได้ 2) แต่ได้ %d", b.Dropped())
	}
}

// เขียนไม่สำเร็จต้องไม่ทำให้ตัวเขียนตาย ไม่งั้น DB สะดุดครั้งเดียวแล้วเลิกนับถาวร
func TestBlockedBufferSurvivesWriteError(t *testing.T) {
	var mu sync.Mutex
	var seen []BlockedInput
	b := NewBlockedBuffer(func(_ context.Context, in BlockedInput) error {
		mu.Lock()
		seen = append(seen, in)
		mu.Unlock()
		return errors.New("DB ล่ม")
	}, 8)
	b.Logf = func(string, ...any) {}

	ctx, cancel := context.WithCancel(context.Background())
	go b.Run(ctx)
	defer cancel()

	for i := 0; i < 3; i++ {
		b.Record(BlockedInput{ClientIP: "1.1.1.1", Path: "/deposit", Reason: BlockedNotInAllowlist})
	}
	drain(t, b, 3, &seen, &mu)
}

// ยกเลิก ctx แล้วต้องคืนค่า ไม่ค้าง goroutine ทิ้งไว้ตอน shutdown
func TestBlockedBufferStopsOnContextCancel(t *testing.T) {
	b := NewBlockedBuffer(func(context.Context, BlockedInput) error { return nil }, 4)
	b.Logf = func(string, ...any) {}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); b.Run(ctx) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run ไม่ยอมคืนค่าหลัง ctx ถูกยกเลิก")
	}
}
