package amqpx

import (
	"sync"

	amqp "github.com/rabbitmq/amqp091-go"
)

// waiters จับคู่ correlation_id กับ goroutine ที่กำลังรอ reply อยู่
// จุดสำคัญคือทุกคนที่ add ต้อง remove เสมอแม้ตอน timeout
// ไม่งั้น map จะโตขึ้นทุก request ที่ไม่ได้รับ reply
type waiters struct {
	mu sync.Mutex
	m  map[string]chan amqp.Delivery
}

func newWaiters() *waiters { return &waiters{m: map[string]chan amqp.Delivery{}} }

// add จอง slot และคืน channel ที่มี buffer 1
// buffer สำคัญ: ทำให้ deliver ไม่ block แม้ caller จะเลิกรอไปแล้วระหว่างนั้น
func (w *waiters) add(corrID string) chan amqp.Delivery {
	ch := make(chan amqp.Delivery, 1)
	w.mu.Lock()
	w.m[corrID] = ch
	w.mu.Unlock()
	return ch
}

func (w *waiters) remove(corrID string) {
	w.mu.Lock()
	delete(w.m, corrID)
	w.mu.Unlock()
}

// deliver ส่ง reply ให้ผู้รอ คืน false ถ้าไม่มีใครรออยู่แล้ว
func (w *waiters) deliver(corrID string, d amqp.Delivery) bool {
	w.mu.Lock()
	ch, ok := w.m[corrID]
	w.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- d:
		return true
	default:
		return false
	}
}

func (w *waiters) len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.m)
}
