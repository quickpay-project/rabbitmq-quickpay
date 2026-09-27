package amqpx

import (
	"sync"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
)

func TestWaiterDeliversToCaller(t *testing.T) {
	w := newWaiters()
	ch := w.add("corr-1")
	if !w.deliver("corr-1", amqp.Delivery{Body: []byte("pong")}) {
		t.Fatal("deliver ต้องสำเร็จ")
	}
	if got := string((<-ch).Body); got != "pong" {
		t.Fatalf("ได้ %q, want pong", got)
	}
}

// Review Focus #1 — entry ต้องหายทุกครั้ง ไม่งั้น memory รั่วทุก request ที่ timeout
func TestWaiterRemoveLeavesNothingBehind(t *testing.T) {
	w := newWaiters()
	w.add("corr-1")
	if w.len() != 1 {
		t.Fatalf("len = %d, want 1", w.len())
	}
	w.remove("corr-1")
	if w.len() != 0 {
		t.Fatalf("len = %d, want 0 — entry ค้างคือ memory leak", w.len())
	}
}

func TestDeliverAfterRemoveDoesNotBlockOrPanic(t *testing.T) {
	w := newWaiters()
	w.add("corr-1")
	w.remove("corr-1")
	// reply มาถึงหลัง caller เลิกรอไปแล้ว — ต้องไม่ block และไม่ panic
	if w.deliver("corr-1", amqp.Delivery{Body: []byte("late")}) {
		t.Fatal("deliver ไปยัง waiter ที่ถูกลบแล้วต้องคืน false")
	}
}

func TestDeliverToUnknownCorrelationIsIgnored(t *testing.T) {
	w := newWaiters()
	if w.deliver("ไม่เคยมี", amqp.Delivery{Body: []byte("x")}) {
		t.Fatal("correlation ที่ไม่รู้จักต้องคืน false")
	}
}

func TestWaitersConcurrentUse(t *testing.T) {
	w := newWaiters()
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := string(rune('a'+n%26)) + string(rune('0'+n/26))
			ch := w.add(id)
			go w.deliver(id, amqp.Delivery{Body: []byte("ok")})
			<-ch
			w.remove(id)
		}(i)
	}
	wg.Wait()
	if w.len() != 0 {
		t.Fatalf("len = %d, want 0", w.len())
	}
}
