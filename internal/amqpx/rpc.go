package amqpx

import (
	"context"
	"errors"
	"sync/atomic"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	directReplyTo = "amq.rabbitmq.reply-to"

	// HeaderUpstreamStatus พา HTTP status ของ upstream กลับมาให้ฝั่ง HTTP
	// ส่งต่อ status code เดิมให้ caller ได้ ตัว body ยังเป็น passthrough ล้วน
	HeaderUpstreamStatus = "x-upstream-status"
)

// amqpChannel คือเฉพาะส่วนของ *amqp.Channel ที่ pool ใช้จริง
// แยกเป็น interface เพื่อทดสอบ error path ตอน setup ได้โดยไม่ต้องมี broker
type amqpChannel interface {
	Consume(queue, consumer string, autoAck, exclusive, noLocal, noWait bool, args amqp.Table) (<-chan amqp.Delivery, error)
	PublishWithContext(ctx context.Context, exchange, key string, mandatory, immediate bool, msg amqp.Publishing) error
	Close() error
}

var _ amqpChannel = (*amqp.Channel)(nil)

type rpcChannel struct {
	ch amqpChannel
	w  *waiters
}

// RPCPool ถือ channel หลายช่อง แต่ละช่อง consume amq.rabbitmq.reply-to ครั้งเดียวตอน start
// ที่ต้องเป็น pool เพราะ amqp091-go ล็อกภายในตอน publish ช่องเดียวจะ serialize ทั้งระบบ
type RPCPool struct {
	chans []*rpcChannel
	next  atomic.Uint64
}

func NewRPCPool(ctx context.Context, m *Manager, size int) (*RPCPool, error) {
	return newRPCPool(size, func() (amqpChannel, error) {
		ch, err := m.Channel(ctx)
		if err != nil {
			return nil, err
		}
		return ch, nil
	})
}

// newRPCPool แยก logic ออกจากการเปิด channel จริง เพื่อให้เทสต์ error path ได้
func newRPCPool(size int, open func() (amqpChannel, error)) (*RPCPool, error) {
	if size < 1 {
		return nil, errors.New("ขนาด pool ต้องอย่างน้อย 1")
	}
	p := &RPCPool{}
	for i := 0; i < size; i++ {
		ch, err := open()
		if err != nil {
			_ = p.Close()
			return nil, err
		}
		// ลงทะเบียนเข้า pool ทันทีที่เปิด channel สำเร็จ ก่อนทำ setup ที่ยังล้มได้
		// Close() จึงเป็นเจ้าของเดียวที่ปิด channel เสมอ ไม่ว่าจะออก error path ตรงไหน
		// ถ้า append ทีหลัง channel ที่ Consume ล้มจะรั่วทิ้งไว้บน broker
		rc := &rpcChannel{ch: ch, w: newWaiters()}
		p.chans = append(p.chans, rc)

		msgs, err := ch.Consume(directReplyTo, "", true, false, false, false, nil)
		if err != nil {
			_ = p.Close()
			return nil, err
		}
		go func(rc *rpcChannel, msgs <-chan amqp.Delivery) {
			// จบเองเมื่อ channel ปิด — ห้าม block ถาวร
			for d := range msgs {
				rc.w.deliver(d.CorrelationId, d)
			}
		}(rc, msgs)
	}
	return p, nil
}

// Call publish แล้วรอ reply ที่ correlation_id ตรงกัน
// pub.CorrelationId ต้องถูกตั้งมาจาก caller และ ReplyTo จะถูกตั้งให้เอง
func (p *RPCPool) Call(ctx context.Context, queue string, pub amqp.Publishing) (*amqp.Delivery, error) {
	if pub.CorrelationId == "" {
		return nil, errors.New("ต้องมี CorrelationId")
	}
	rc := p.chans[int(p.next.Add(1))%len(p.chans)]

	ch := rc.w.add(pub.CorrelationId)
	// remove เสมอไม่ว่าจะออกทางไหน — นี่คือสิ่งที่กัน map โตไม่รู้จบ
	defer rc.w.remove(pub.CorrelationId)

	pub.ReplyTo = directReplyTo
	if err := rc.ch.PublishWithContext(ctx, "", queue, false, false, pub); err != nil {
		return nil, err
	}

	select {
	case d := <-ch:
		return &d, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Pending บอกจำนวน caller ที่ยังรอ reply อยู่ ใช้เฝ้า leak
func (p *RPCPool) Pending() int {
	n := 0
	for _, rc := range p.chans {
		n += rc.w.len()
	}
	return n
}

func (p *RPCPool) Close() error {
	var firstErr error
	for _, rc := range p.chans {
		if err := rc.ch.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
