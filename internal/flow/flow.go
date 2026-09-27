package flow

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/celalsahinaltinisik/internal/model"
	amqp "github.com/rabbitmq/amqp091-go"
)

// Broker คือส่วนที่คุยกับ RabbitMQ แยกเป็น interface เพื่อให้ทดสอบ lifecycle ได้โดยไม่ต้องมี broker
type Broker interface {
	DeclareQueue(name string) error
	Consume(queue string, prefetch int) (<-chan amqp.Delivery, string, error)
	Cancel(tag string) error
	Close() error
}

type Options struct {
	Spec    model.GroupSpec
	Queue   string
	Broker  Broker
	Process func(ctx context.Context, d amqp.Delivery, spec model.GroupSpec)
	Logf    func(string, ...any)
}

// Flow คือหน่วยที่ reconciler สั่งการ: 1 group = 1 queue + 1 consumer + worker pool
type Flow struct {
	opts Options
	spec atomic.Pointer[model.GroupSpec]

	mu       sync.Mutex
	tag      string
	done     chan struct{}
	draining bool
}

func New(o Options) *Flow {
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	f := &Flow{opts: o, done: make(chan struct{})}
	s := o.Spec
	f.spec.Store(&s)
	return f
}

func (f *Flow) Spec() model.GroupSpec { return *f.spec.Load() }

// UpdateSpec สลับ spec แบบ atomic — worker อ่านค่าใหม่ในข้อความถัดไปโดยไม่ต้อง restart
func (f *Flow) UpdateSpec(s model.GroupSpec) { f.spec.Store(&s) }

// Run บล็อกจนกว่า channel ของ broker จะปิด แล้ว return เสมอ
//
// ห้ามใส่ select {} หรือ block ถาวรตรงนี้เด็ดขาด ระบบเก่าทำแบบนั้นที่
// conswithdraw.go:288 ทำให้ตอน AMQP หลุด worker ออกหมดแต่ goroutine ค้างถาวร
// supervisor จึงไม่เคยรู้ว่ามันตายและ readiness ยังรายงานว่าปกติ
func (f *Flow) Run(ctx context.Context) error {
	defer func() {
		f.mu.Lock()
		select {
		case <-f.done:
		default:
			close(f.done)
		}
		f.mu.Unlock()
	}()

	if err := f.opts.Broker.DeclareQueue(f.opts.Queue); err != nil {
		return err
	}

	spec := f.Spec()
	prefetch := spec.WorkerCount
	if prefetch < 1 {
		prefetch = 1
	}

	msgs, tag, err := f.opts.Broker.Consume(f.opts.Queue, prefetch)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.tag = tag
	f.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < prefetch; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range msgs {
				f.opts.Process(ctx, d, f.Spec())
			}
		}()
	}
	wg.Wait()
	return nil
}

// Drain หยุดรับงานใหม่แล้วรอให้ของในมือจบภายใน timeout
func (f *Flow) Drain(timeout time.Duration) error {
	f.mu.Lock()
	tag := f.tag
	f.draining = true
	f.mu.Unlock()

	if tag != "" {
		if err := f.opts.Broker.Cancel(tag); err != nil {
			f.opts.Logf("⚠️  cancel consumer %s ไม่สำเร็จ: %v", tag, err)
		}
	}

	select {
	case <-f.done:
		return nil
	case <-time.After(timeout):
		return errors.New("drain ไม่จบภายในเวลาที่กำหนด — ข้อความที่ยังไม่ ack จะถูก requeue")
	}
}

func (f *Flow) Draining() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.draining
}
