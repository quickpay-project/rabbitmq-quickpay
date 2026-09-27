package amqpx

import (
	"context"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Manager ถือ connection เดียวของทั้ง process และต่อใหม่ให้เองเมื่อหลุด
// แทนพฤติกรรมเดิมที่เปิด connection ใหม่ทุก HTTP request
type Manager struct {
	url  string
	Logf func(string, ...any)

	mu   sync.Mutex
	conn *amqp.Connection
}

func NewManager(url string) *Manager {
	return &Manager{url: url, Logf: func(string, ...any) {}}
}

// connection คืน connection ที่ยังใช้ได้ ถ้าไม่มีจะต่อใหม่พร้อม backoff
func (m *Manager) connection(ctx context.Context) (*amqp.Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.conn != nil && !m.conn.IsClosed() {
		return m.conn, nil
	}

	var conn *amqp.Connection
	err := Retry(ctx, time.Second, 30*time.Second,
		func(d time.Duration) {
			m.Logf("⏳ ต่อ RabbitMQ ไม่ได้ ลองใหม่ใน %v", d)
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		},
		func() error {
			c, err := amqp.Dial(m.url)
			if err != nil {
				return err
			}
			conn = c
			return nil
		})
	if err != nil {
		return nil, err
	}
	m.conn = conn
	return conn, nil
}

// Channel เปิด channel ใหม่บน connection ที่ใช้ร่วมกัน
// channel ถูกที่จะเปิดหลายอัน ต่างจาก connection
func (m *Manager) Channel(ctx context.Context) (*amqp.Channel, error) {
	conn, err := m.connection(ctx)
	if err != nil {
		return nil, err
	}
	return conn.Channel()
}

// Healthy บอกว่า connection ที่ใช้ร่วมกันยังใช้งานได้จริงหรือไม่ ใช้ตอบ /readyz
//
// จำเป็นเพราะ RPCPool เปิด channel ครั้งเดียวตอน start แล้วไม่เคยเปิดใหม่ (rpc.go)
// ส่วน connection จะ redial ก็ต่อเมื่อมีคนเรียก Channel() ซึ่ง pool ไม่เคยเรียกอีก
// เมื่อ broker restart หรือ TCP หลุด ทุก Call จะได้ ErrClosed **ถาวร** จนกว่าจะ restart
// process ขณะที่ฝั่ง consumer กู้ตัวเองได้ (msgs ปิด → Run คืนค่า → reconcile Restart)
// ทำให้ Registry.AllRunning ยังคืน true แล้ว /readyz กลับมาเขียวทั้งที่เส้นทาง request
// ตายสนิท — เป็นบาปเดียวกับที่ spec §6.5 ชี้ว่าร้ายแรงที่สุดของระบบเก่า
// readiness จึงต้องดูสุขภาพ AMQP ตรง ๆ เพื่อให้ orchestrator restart pod
// แทนที่จะ route traffic เข้าหลุมดำ
//
// TODO(auto-reconnect): ทางแก้ที่ถูกจริงคือให้ RPCPool กู้ channel ของตัวเองด้วย
// amqp.Channel.NotifyClose แล้วเปิดใหม่ผ่าน Manager.Channel พร้อม re-consume
// amq.rabbitmq.reply-to ของช่องนั้น ทดสอบได้โดยไม่ต้องมี broker จริงเพราะ pool คุยกับ
// amqpChannel interface (rpc.go) อยู่แล้ว และ newRPCPool รับ open func() (amqpChannel, error)
// ซึ่ง fake ได้ตรง ๆ — รอบนี้ทำแค่ให้ readiness บอกความจริง
func (m *Manager) Healthy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conn != nil && !m.conn.IsClosed()
}

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn == nil || m.conn.IsClosed() {
		return nil
	}
	return m.conn.Close()
}
