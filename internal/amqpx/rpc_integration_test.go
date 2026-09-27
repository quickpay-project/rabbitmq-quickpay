//go:build integration

package amqpx

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	url := os.Getenv("TEST_RABBITMQ_URL")
	if url == "" {
		t.Skip("ตั้ง TEST_RABBITMQ_URL เพื่อรัน integration test")
	}
	m := NewManager(url)
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestRPCRoundTrip(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	queue := fmt.Sprintf("mqtest_%d", time.Now().UnixNano())

	srvCh, err := m.Channel(ctx)
	if err != nil {
		t.Fatalf("เปิด channel: %v", err)
	}
	defer srvCh.Close()
	if _, err := srvCh.QueueDeclare(queue, true, false, false, false, nil); err != nil {
		t.Fatalf("declare queue: %v", err)
	}
	defer func() { _, _ = srvCh.QueueDelete(queue, false, false, false) }()

	msgs, err := srvCh.Consume(queue, "", false, false, false, false, nil)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	go func() {
		for d := range msgs {
			_ = srvCh.PublishWithContext(context.Background(), "", d.ReplyTo, false, false,
				amqp.Publishing{CorrelationId: d.CorrelationId, Body: []byte("pong:" + string(d.Body))})
			_ = d.Ack(false)
		}
	}()

	pool, err := NewRPCPool(ctx, m, 2)
	if err != nil {
		t.Fatalf("NewRPCPool: %v", err)
	}
	defer pool.Close()

	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	reply, err := pool.Call(callCtx, queue,
		amqp.Publishing{CorrelationId: "corr-1", Body: []byte("ping")})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if string(reply.Body) != "pong:ping" {
		t.Fatalf("ได้ %q, want pong:ping", reply.Body)
	}
	if pool.Pending() != 0 {
		t.Errorf("Pending = %d, want 0", pool.Pending())
	}
}

// Review Focus #1 — timeout แล้วต้องไม่เหลือ entry ค้าง
func TestRPCTimeoutLeavesNoPendingEntry(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	queue := fmt.Sprintf("mqtest_noconsumer_%d", time.Now().UnixNano())

	ch, err := m.Channel(ctx)
	if err != nil {
		t.Fatalf("เปิด channel: %v", err)
	}
	if _, err := ch.QueueDeclare(queue, true, false, false, false, nil); err != nil {
		t.Fatalf("declare queue: %v", err)
	}
	defer func() { _, _ = ch.QueueDelete(queue, false, false, false); ch.Close() }()

	pool, err := NewRPCPool(ctx, m, 1)
	if err != nil {
		t.Fatalf("NewRPCPool: %v", err)
	}
	defer pool.Close()

	for i := 0; i < 20; i++ {
		callCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		_, err := pool.Call(callCtx, queue,
			amqp.Publishing{CorrelationId: fmt.Sprintf("corr-%d", i), Body: []byte("x")})
		cancel()
		if err == nil {
			t.Fatal("ไม่มี consumer ต้อง timeout")
		}
	}
	if n := pool.Pending(); n != 0 {
		t.Fatalf("Pending = %d หลัง timeout 20 ครั้ง, want 0 — นี่คือ memory leak", n)
	}
}
