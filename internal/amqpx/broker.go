package amqpx

import (
	"context"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Broker คือ channel หนึ่งช่องที่ flow หนึ่งตัวใช้
// ต้องแยกช่องต่อ flow เพราะ Qos (prefetch) เป็นค่าระดับ channel
type Broker struct {
	ch *amqp.Channel
}

func NewBroker(ctx context.Context, m *Manager) (*Broker, error) {
	ch, err := m.Channel(ctx)
	if err != nil {
		return nil, err
	}
	return &Broker{ch: ch}, nil
}

// DeclareQueue ประกาศ queue แบบ durable
//
// durable=true ให้ตัวนิยาม queue รอดข้าม broker restart ซึ่งถูกมากเพราะไม่มี fsync ต่อข้อความ
// ส่วนข้อความไม่ตั้ง persistent โดยตั้งใจ — ถ้า broker restart ข้อความที่รอดมาก็เลย
// x-deadline ไปแล้วทั้งหมด การเก็บมันไว้จึงไม่มีประโยชน์
func (b *Broker) DeclareQueue(name string) error {
	_, err := b.ch.QueueDeclare(name, true, false, false, false, nil)
	return err
}

func (b *Broker) Consume(queue string, prefetch int) (<-chan amqp.Delivery, string, error) {
	if err := b.ch.Qos(prefetch, 0, false); err != nil {
		return nil, "", err
	}
	tag := "gw-" + queue
	msgs, err := b.ch.Consume(queue, tag, false, false, false, false, nil)
	if err != nil {
		return nil, "", err
	}
	return msgs, tag, nil
}

// Cancel สั่ง broker หยุดส่งข้อความใหม่ — msgs จะปิดเองหลัง delivery สุดท้าย
func (b *Broker) Cancel(tag string) error { return b.ch.Cancel(tag, false) }

func (b *Broker) Publish(ctx context.Context, queue string, pub amqp.Publishing) error {
	return b.ch.PublishWithContext(ctx, "", queue, false, false, pub)
}

func (b *Broker) Close() error { return b.ch.Close() }
