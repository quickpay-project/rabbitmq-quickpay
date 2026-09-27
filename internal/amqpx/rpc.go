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

type rpcChannel struct {
	ch *amqp.Channel
	w  *waiters
}

// RPCPool ถือ channel หลายช่อง แต่ละช่อง consume amq.rabbitmq.reply-to ครั้งเดียวตอน start
// ที่ต้องเป็น pool เพราะ amqp091-go ล็อกภายในตอน publish ช่องเดียวจะ serialize ทั้งระบบ
type RPCPool struct {
	chans []*rpcChannel
	next  atomic.Uint64
}

func NewRPCPool(ctx context.Context, m *Manager, size int) (*RPCPool, error) {
	if size < 1 {
		return nil, errors.New("ขนาด pool ต้องอย่างน้อย 1")
	}
	p := &RPCPool{}
	for i := 0; i < size; i++ {
		ch, err := m.Channel(ctx)
		if err != nil {
			_ = p.Close()
			return nil, err
		}
		rc := &rpcChannel{ch: ch, w: newWaiters()}

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

		p.chans = append(p.chans, rc)
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
