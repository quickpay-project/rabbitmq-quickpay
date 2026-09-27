package amqpx

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// fakeChannel แทน *amqp.Channel เพื่อทดสอบ setup path ของ pool โดยไม่ต้องมี broker
type fakeChannel struct {
	consumeErr error
	publishErr error

	msgs      chan amqp.Delivery
	closeOnce sync.Once
	closes    atomic.Int32

	mu        sync.Mutex
	published []amqp.Publishing
}

func newFakeChannel() *fakeChannel {
	return &fakeChannel{msgs: make(chan amqp.Delivery, 1)}
}

func (f *fakeChannel) Consume(queue, consumer string, autoAck, exclusive, noLocal, noWait bool, args amqp.Table) (<-chan amqp.Delivery, error) {
	if f.consumeErr != nil {
		return nil, f.consumeErr
	}
	return f.msgs, nil
}

func (f *fakeChannel) PublishWithContext(ctx context.Context, exchange, key string, mandatory, immediate bool, msg amqp.Publishing) error {
	if f.publishErr != nil {
		return f.publishErr
	}
	f.mu.Lock()
	f.published = append(f.published, msg)
	f.mu.Unlock()
	return nil
}

func (f *fakeChannel) Close() error {
	f.closes.Add(1)
	f.closeOnce.Do(func() { close(f.msgs) })
	return nil
}

func (f *fakeChannel) closeCount() int { return int(f.closes.Load()) }

// Review finding (Important) — channel ที่เปิดสำเร็จแล้วแต่ Consume ล้ม
// ต้องถูกปิด ไม่ใช่ปล่อยรั่วทิ้งไว้บน broker
func TestNewRPCPoolClosesChannelWhenConsumeFails(t *testing.T) {
	boom := errors.New("consume ไม่ผ่าน")
	var opened []*fakeChannel

	pool, err := newRPCPool(1, func() (amqpChannel, error) {
		f := newFakeChannel()
		f.consumeErr = boom
		opened = append(opened, f)
		return f, nil
	})
	if pool != nil {
		t.Error("pool ต้องเป็น nil เมื่อ setup ล้ม")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if len(opened) != 1 {
		t.Fatalf("เปิด channel %d อัน, want 1", len(opened))
	}
	if n := opened[0].closeCount(); n != 1 {
		t.Fatalf("channel ถูกปิด %d ครั้ง, want 1 — channel ที่เปิดแล้วรั่วทิ้งบน broker", n)
	}
}

// Consume ของตัวที่สองล้ม — ตัวแรกที่ตั้งสำเร็จแล้วก็ต้องถูกปิดด้วย
func TestNewRPCPoolClosesEarlierChannelsWhenLaterConsumeFails(t *testing.T) {
	boom := errors.New("consume ตัวที่สองล้ม")
	var opened []*fakeChannel

	_, err := newRPCPool(2, func() (amqpChannel, error) {
		f := newFakeChannel()
		if len(opened) == 1 {
			f.consumeErr = boom
		}
		opened = append(opened, f)
		return f, nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if len(opened) != 2 {
		t.Fatalf("เปิด channel %d อัน, want 2", len(opened))
	}
	for i, f := range opened {
		if n := f.closeCount(); n != 1 {
			t.Errorf("channel[%d] ถูกปิด %d ครั้ง, want 1", i, n)
		}
	}
}

// เปิด channel ตัวที่สองไม่ได้เลย — ตัวแรกต้องไม่ค้าง
func TestNewRPCPoolClosesEarlierChannelsWhenOpenFails(t *testing.T) {
	boom := errors.New("เปิด channel ไม่ได้")
	var opened []*fakeChannel

	_, err := newRPCPool(2, func() (amqpChannel, error) {
		if len(opened) == 1 {
			return nil, boom
		}
		f := newFakeChannel()
		opened = append(opened, f)
		return f, nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if len(opened) != 1 {
		t.Fatalf("เปิด channel สำเร็จ %d อัน, want 1", len(opened))
	}
	if n := opened[0].closeCount(); n != 1 {
		t.Fatalf("channel[0] ถูกปิด %d ครั้ง, want 1", n)
	}
}

func TestNewRPCPoolRejectsSizeBelowOne(t *testing.T) {
	called := 0
	if _, err := newRPCPool(0, func() (amqpChannel, error) {
		called++
		return newFakeChannel(), nil
	}); err == nil {
		t.Fatal("size 0 ต้องคืน error")
	}
	if called != 0 {
		t.Errorf("เปิด channel %d ครั้ง — size ไม่ถูกต้องต้องไม่เปิดอะไรเลย", called)
	}
}

// success path — ยืนยันว่าการจัดลำดับ register/consume ใหม่ไม่ทำให้สายไฟหลุด
func TestRPCPoolCallDeliversReplyAndLeavesNoPending(t *testing.T) {
	f := newFakeChannel()
	pool, err := newRPCPool(1, func() (amqpChannel, error) { return f, nil })
	if err != nil {
		t.Fatalf("newRPCPool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	var reply *amqp.Delivery
	var callErr error
	go func() {
		defer close(done)
		reply, callErr = pool.Call(ctx, "q", amqp.Publishing{CorrelationId: "corr-1", Body: []byte("ping")})
	}()

	// รอให้ Call ลงทะเบียน waiter เสร็จก่อนป้อน reply เข้าไป
	deadline := time.Now().Add(2 * time.Second)
	for pool.Pending() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Call ไม่ลงทะเบียน waiter")
		}
		time.Sleep(time.Millisecond)
	}
	f.msgs <- amqp.Delivery{CorrelationId: "corr-1", Body: []byte("pong")}
	<-done

	if callErr != nil {
		t.Fatalf("Call: %v", callErr)
	}
	if got := string(reply.Body); got != "pong" {
		t.Fatalf("ได้ %q, want pong", got)
	}
	if n := pool.Pending(); n != 0 {
		t.Errorf("Pending = %d, want 0", n)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.published) != 1 {
		t.Fatalf("publish %d ครั้ง, want 1", len(f.published))
	}
	if f.published[0].ReplyTo != directReplyTo {
		t.Errorf("ReplyTo = %q, want %q", f.published[0].ReplyTo, directReplyTo)
	}
}
