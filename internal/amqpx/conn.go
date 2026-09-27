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

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn == nil || m.conn.IsClosed() {
		return nil
	}
	return m.conn.Close()
}
