package amqpx

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetrySucceedsFirstTry(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), time.Second, time.Minute,
		func(time.Duration) {}, func() error { calls++; return nil })
	if err != nil || calls != 1 {
		t.Fatalf("err=%v calls=%d, want nil/1", err, calls)
	}
}

func TestRetryBacksOffExponentiallyUpToMax(t *testing.T) {
	var slept []time.Duration
	calls := 0
	_ = Retry(context.Background(), time.Second, 4*time.Second,
		func(d time.Duration) { slept = append(slept, d) },
		func() error {
			calls++
			if calls < 5 {
				return errors.New("ยังต่อไม่ได้")
			}
			return nil
		})
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second}
	if len(slept) != len(want) {
		t.Fatalf("sleep %v, want %v", slept, want)
	}
	for i := range want {
		if slept[i] != want[i] {
			t.Errorf("sleep[%d] = %v, want %v", i, slept[i], want[i])
		}
	}
}

func TestRetryStopsWhenContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := Retry(ctx, time.Millisecond, time.Millisecond,
		func(time.Duration) { cancel() },
		func() error { calls++; return errors.New("ล้ม") })
	if err == nil {
		t.Fatal("ต้องคืน error เมื่อ context ถูกยกเลิก")
	}
	if calls > 2 {
		t.Errorf("เรียก fn %d ครั้ง — ต้องหยุดทันทีที่ ctx ถูกยกเลิก", calls)
	}
}
