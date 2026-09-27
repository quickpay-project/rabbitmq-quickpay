package flow

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/celalsahinaltinisik/internal/forward"
	"github.com/celalsahinaltinisik/internal/model"
	"github.com/celalsahinaltinisik/internal/store"
	amqp "github.com/rabbitmq/amqp091-go"
)

type fakeSender struct {
	called atomic.Bool
	result forward.Result
	panics bool
	gotHdr http.Header // header ที่ processor แปลงมาให้ — ใช้ตรวจว่าส่งต่อครบ
}

func (f *fakeSender) Send(_ context.Context, _ model.GroupSpec, _ []byte,
	hdr http.Header, _ time.Time) forward.Result {
	f.called.Store(true)
	f.gotHdr = hdr
	if f.panics {
		panic("upstream client ระเบิด")
	}
	return f.result
}

type fakeRecorder struct {
	mu       sync.Mutex
	attempts []store.AttemptInput
	finished []store.FinishRequestInput
}

func (r *fakeRecorder) RecordAttempts(_ context.Context, _ string, a []store.AttemptInput) error {
	r.mu.Lock()
	r.attempts = append(r.attempts, a...)
	r.mu.Unlock()
	return nil
}

func (r *fakeRecorder) FinishRequest(_ context.Context, in store.FinishRequestInput) error {
	r.mu.Lock()
	r.finished = append(r.finished, in)
	r.mu.Unlock()
	return nil
}

func (r *fakeRecorder) lastStatus(t *testing.T) model.RequestStatus {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.finished) == 0 {
		t.Fatal("ไม่มีการเรียก FinishRequest เลย")
	}
	return r.finished[len(r.finished)-1].Status
}

func delivery(ack amqp.Acknowledger, traceID string, deadline time.Time, replyTo string) amqp.Delivery {
	h := amqp.Table{}
	if traceID != "" {
		h[HeaderTraceID] = traceID
	}
	if !deadline.IsZero() {
		h[HeaderDeadline] = strconv.FormatInt(deadline.UnixMilli(), 10)
	}
	return amqp.Delivery{
		Acknowledger:  ack,
		Headers:       h,
		CorrelationId: "corr-1",
		ReplyTo:       replyTo,
		Body:          []byte(`{"amount":100}`),
	}
}

func okResult() forward.Result {
	r := forward.Result{Attempts: []forward.Attempt{
		{Seq: 1, URLID: 1, URL: "https://a", HTTPStatus: 200,
			Outcome: model.OutcomeSuccess, Body: []byte(`{"code":0}`), Duration: 5 * time.Millisecond},
	}}
	r.Final = &r.Attempts[0]
	return r
}

func TestHandleSuccessLogsRepliesAndAcks(t *testing.T) {
	ack := &fakeAck{}
	snd := &fakeSender{result: okResult()}
	rec := &fakeRecorder{}
	var published []amqp.Publishing

	p := NewProcessor(snd, rec, func(_ context.Context, _ string, pub amqp.Publishing) error {
		published = append(published, pub)
		return nil
	})

	p.Handle(context.Background(),
		delivery(ack, "trace-1", time.Now().Add(time.Minute), "reply-q"),
		testSpec(1))

	if rec.lastStatus(t) != model.StatusSuccess {
		t.Errorf("status = %q, want success", rec.lastStatus(t))
	}
	if len(rec.attempts) != 1 {
		t.Errorf("บันทึก attempt %d แถว, want 1", len(rec.attempts))
	}
	if len(published) != 1 || string(published[0].Body) != `{"code":0}` {
		t.Errorf("reply = %+v", published)
	}
	if published[0].CorrelationId != "corr-1" {
		t.Error("reply ต้องแนบ correlation_id เดิม ไม่งั้น caller จับคู่ไม่ได้")
	}
	if ack.acked.Load() != 1 {
		t.Errorf("ack %d ครั้ง, want 1", ack.acked.Load())
	}
}

// Review Focus #2 — deadline ที่หายหรือพัง ต้องไม่ทำให้ยิง upstream แบบไร้ขอบเขต
func TestHandleTreatsMissingOrBadDeadlineAsExpired(t *testing.T) {
	for _, name := range []string{"หาย", "พัง"} {
		ack := &fakeAck{}
		snd := &fakeSender{result: okResult()}
		rec := &fakeRecorder{}

		d := delivery(ack, "trace-1", time.Time{}, "reply-q")
		if name == "พัง" {
			d.Headers[HeaderDeadline] = "ไม่ใช่ตัวเลข"
		}

		NewProcessor(snd, rec, func(context.Context, string, amqp.Publishing) error { return nil }).
			Handle(context.Background(), d, testSpec(1))

		if snd.called.Load() {
			t.Errorf("deadline %s: ห้ามยิง upstream", name)
		}
		if rec.lastStatus(t) != model.StatusExpired {
			t.Errorf("deadline %s: status = %q, want expired", name, rec.lastStatus(t))
		}
		if ack.acked.Load() != 1 {
			t.Errorf("deadline %s: ต้อง ack ทิ้ง ไม่ปล่อยให้วนซ้ำ", name)
		}
	}
}

func TestHandleSkipsUpstreamWhenDeadlinePassed(t *testing.T) {
	ack := &fakeAck{}
	snd := &fakeSender{result: okResult()}
	rec := &fakeRecorder{}
	replied := false

	NewProcessor(snd, rec, func(context.Context, string, amqp.Publishing) error {
		replied = true
		return nil
	}).Handle(context.Background(),
		delivery(ack, "trace-1", time.Now().Add(-time.Second), "reply-q"), testSpec(1))

	if snd.called.Load() {
		t.Fatal("deadline ผ่านแล้วห้ามยิง upstream — นี่คือกลไกกันออเดอร์ผี")
	}
	if replied {
		t.Error("ไม่ต้อง reply เพราะไม่มีใครรออยู่แล้ว")
	}
	if rec.lastStatus(t) != model.StatusExpired {
		t.Errorf("status = %q, want expired", rec.lastStatus(t))
	}
}

func TestHandleNoUpstreamWhenGroupHasNoURL(t *testing.T) {
	ack := &fakeAck{}
	snd := &fakeSender{result: forward.Result{}}
	rec := &fakeRecorder{}
	var published []amqp.Publishing

	spec := testSpec(1)
	spec.URLs = nil

	NewProcessor(snd, rec, func(_ context.Context, _ string, pub amqp.Publishing) error {
		published = append(published, pub)
		return nil
	}).Handle(context.Background(),
		delivery(ack, "trace-1", time.Now().Add(time.Minute), "reply-q"), spec)

	if rec.lastStatus(t) != model.StatusNoUpstream {
		t.Errorf("status = %q, want no_upstream", rec.lastStatus(t))
	}
	if len(published) != 1 {
		t.Fatal("ต้อง reply บอก caller ว่าไม่มีปลายทาง ไม่ใช่ปล่อยให้รอจน timeout")
	}
	if ack.acked.Load() != 1 {
		t.Error("ต้อง ack")
	}
}

func TestHandleFatalOutcomeStillRepliesAndAcks(t *testing.T) {
	ack := &fakeAck{}
	res := forward.Result{Attempts: []forward.Attempt{
		{Seq: 1, URL: "https://a", HTTPStatus: 400, Outcome: model.OutcomeFatal,
			Body: []byte(`{"message":"ยอดเงินไม่พอ"}`)},
	}}
	res.Final = &res.Attempts[0]
	rec := &fakeRecorder{}
	var published []amqp.Publishing

	NewProcessor(&fakeSender{result: res}, rec,
		func(_ context.Context, _ string, pub amqp.Publishing) error {
			published = append(published, pub)
			return nil
		}).Handle(context.Background(),
		delivery(ack, "trace-1", time.Now().Add(time.Minute), "reply-q"), testSpec(1))

	if rec.lastStatus(t) != model.StatusFailed {
		t.Errorf("status = %q, want failed", rec.lastStatus(t))
	}
	if len(published) != 1 || string(published[0].Body) != `{"message":"ยอดเงินไม่พอ"}` {
		t.Error("ต้องส่ง response ของ upstream กลับไปตรง ๆ ให้ caller เห็นเหตุผล")
	}
	if ack.acked.Load() != 1 {
		t.Error("ล้มเหลวก็ต้อง ack — ไม่ requeue เพราะ caller รอแบบ synchronous")
	}
}

func TestHandleRecoversFromPanicAndAcks(t *testing.T) {
	ack := &fakeAck{}
	rec := &fakeRecorder{}

	NewProcessor(&fakeSender{panics: true}, rec,
		func(context.Context, string, amqp.Publishing) error { return nil }).
		Handle(context.Background(),
			delivery(ack, "trace-1", time.Now().Add(time.Minute), "reply-q"), testSpec(1))

	if ack.acked.Load() != 1 {
		t.Fatal("panic แล้วต้อง ack ไม่งั้น poison message จะวนไม่รู้จบ")
	}
	if rec.lastStatus(t) != model.StatusFailed {
		t.Errorf("status = %q, want failed", rec.lastStatus(t))
	}
}

func TestHandleWithoutReplyToSkipsPublishButStillFinishes(t *testing.T) {
	ack := &fakeAck{}
	rec := &fakeRecorder{}
	replied := false

	NewProcessor(&fakeSender{result: okResult()}, rec,
		func(context.Context, string, amqp.Publishing) error { replied = true; return nil }).
		Handle(context.Background(),
			delivery(ack, "trace-1", time.Now().Add(time.Minute), ""), testSpec(1))

	if replied {
		t.Error("ไม่มี ReplyTo ต้องไม่ publish")
	}
	if rec.lastStatus(t) != model.StatusSuccess {
		t.Error("ยังต้องบันทึกผลตามปกติ")
	}
	if ack.acked.Load() != 1 {
		t.Error("ต้อง ack")
	}
}

// panicRecorder ทำให้ FinishRequest panic เพื่อจำลอง "panic ซ้อน" ที่เกิดขึ้น
// ภายในตัวจัดการ recover เอง — กรณีที่ทำให้ ack หลุดถ้า ack อยู่ defer เดียวกัน
type panicRecorder struct {
	fakeRecorder
	calls atomic.Int64
}

func (r *panicRecorder) FinishRequest(context.Context, store.FinishRequestInput) error {
	r.calls.Add(1)
	panic("recorder ระเบิดระหว่างบันทึกผลของ panic แรก")
}

// Fix review finding #1 — panic ซ้อนใน recovery handler ต้องไม่ทำให้ข้อความไม่ถูก ack
// ถ้า ack อยู่ใน defer เดียวกับ recover ข้อความนี้จะกลายเป็น poison message ที่วนไม่รู้จบ
func TestHandleAcksEvenWhenRecoveryHandlerPanics(t *testing.T) {
	ack := &fakeAck{}
	rec := &panicRecorder{}

	NewProcessor(&fakeSender{panics: true}, rec,
		func(context.Context, string, amqp.Publishing) error { return nil }).
		Handle(context.Background(),
			delivery(ack, "trace-1", time.Now().Add(time.Minute), "reply-q"), testSpec(1))

	if rec.calls.Load() != 1 {
		t.Fatalf("FinishRequest ถูกเรียก %d ครั้ง, want 1 — ต้องเข้า recovery handler จริง",
			rec.calls.Load())
	}
	if ack.acked.Load() != 1 {
		t.Fatalf("ack %d ครั้ง, want 1 — panic ซ้อนใน recovery handler ต้องไม่ทำให้ ack หลุด",
			ack.acked.Load())
	}
}

// Fix review finding #2 — header ของ caller ที่มาเป็น []byte ต้องไม่หายเงียบ ๆ
func TestHandleForwardsByteSliceHeadersToUpstream(t *testing.T) {
	ack := &fakeAck{}
	snd := &fakeSender{result: okResult()}
	rec := &fakeRecorder{}

	d := delivery(ack, "trace-1", time.Now().Add(time.Minute), "reply-q")
	d.Headers["x-merchant-id"] = []byte("M001") // long-string จาก broker มาเป็น []byte
	d.Headers["authorization"] = "Bearer abc123"

	NewProcessor(snd, rec, func(context.Context, string, amqp.Publishing) error { return nil }).
		Handle(context.Background(), d, testSpec(1))

	if got := snd.gotHdr.Get("X-Merchant-Id"); got != "M001" {
		t.Errorf("header []byte = %q, want M001 — ต้องไม่ถูกทิ้งเงียบ ๆ", got)
	}
	if got := snd.gotHdr.Get("Authorization"); got != "Bearer abc123" {
		t.Errorf("header string = %q, want Bearer abc123", got)
	}
	if snd.gotHdr.Get(HeaderTraceID) != "" || snd.gotHdr.Get(HeaderDeadline) != "" {
		t.Error("header ภายในของเราเอง (x-trace-id, x-deadline) ต้องไม่ถูกส่งต่อไป upstream")
	}
}
