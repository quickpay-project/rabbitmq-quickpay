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
//
// After Cancel, the Broker MUST close the delivery channel, otherwise Run never returns.
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

	mu        sync.Mutex
	started   bool // set to true before DeclareQueue to guard re-use
	cancelled bool // set to true before first Cancel to de-duplicate calls
	tag       string
	done      chan struct{}
	draining  bool
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
// conswithdraw.go:284 ทำให้ตอน AMQP หลุด worker ออกหมดแต่ goroutine ค้างถาวร
// supervisor จึงไม่เคยรู้ว่ามันตายและ readiness ยังรายงานว่าปกติ
//
// ctx is passed through to Process and also used to cancel the consumer (via watchdog).
// Run is single-use per Flow: calling it twice returns an error on the second call.
func (f *Flow) Run(ctx context.Context) error {
	// Guard against re-use: mark started under lock before any broker call
	f.mu.Lock()
	if f.started {
		f.mu.Unlock()
		return errors.New("Flow.Run called more than once — flows are single-use, create a new Flow")
	}
	f.started = true
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		select {
		case <-f.done:
		default:
			close(f.done)
		}
		f.mu.Unlock()
	}()

	// Flow เป็นเจ้าของ Broker ตัวนี้ผู้เดียว (factory สร้าง channel ใหม่ให้ทุก flow)
	// จึงต้องคืนมันตอนจบ ไม่งั้นทุกการ restart/stop จะทิ้ง AMQP channel ค้างบน
	// connection ที่ใช้ร่วมกันจนชน channel_max (default 2047) แล้วเปิด flow ใหม่ไม่ได้อีก
	// ซึ่งเกิดง่ายมากในระบบนี้เพราะ config เปลี่ยนได้ตลอดผ่าน DB
	//
	// ปิดที่นี่เพราะ Run เป็นจุดเดียวที่ครบทั้งสามเงื่อนไข:
	//   1. ครอบทุกทางออก รวมทาง DeclareQueue/Consume ล้มตั้งแต่เริ่ม (ทางที่รั่วเร็วที่สุด
	//      เพราะ reconciler จะ mark failed แล้วสร้างใหม่ทุกรอบ)
	//   2. รับประกันว่า worker ทุกตัวจบแล้ว — defer นี้ทำงานหลัง wg.Wait() เสมอ
	//      ต่างจากการปิดใน reconcile.stop ที่ Drain อาจ timeout ขณะ worker ยังถือข้อความอยู่
	//   3. ปิดได้ครั้งเดียวแน่นอน เพราะ single-use guard ด้านบนทำให้มาถึงบรรทัดนี้ได้ครั้งเดียว
	//
	// ลงทะเบียนหลัง defer ที่ปิด f.done เพื่อให้ลำดับ LIFO ปิด broker "ก่อน" ส่งสัญญาณ done
	// แปลว่าเมื่อ Drain คืนค่าสำเร็จ channel ถูกคืนเรียบร้อยแล้ว
	defer func() { _ = f.opts.Broker.Close() }()

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

	// Publish tag and check if Drain already called — must do both atomically.
	// If Drain won the race, it saw tag == "" and skipped Cancel, so we do it now.
	f.mu.Lock()
	f.tag = tag
	draining := f.draining
	shouldCancel := !f.cancelled && draining
	if shouldCancel {
		f.cancelled = true
	}
	f.mu.Unlock()
	if shouldCancel && tag != "" {
		_ = f.opts.Broker.Cancel(tag)
	}

	// Watchdog: if ctx cancels, stop pulling new messages
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			f.mu.Lock()
			shouldCancel := !f.cancelled && tag != ""
			if shouldCancel {
				f.cancelled = true
			}
			f.mu.Unlock()
			if shouldCancel {
				_ = f.opts.Broker.Cancel(tag)
			}
		case <-done:
		}
	}()

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
	shouldCancel := !f.cancelled && tag != ""
	if shouldCancel {
		f.cancelled = true
	}
	f.mu.Unlock()

	if shouldCancel {
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
