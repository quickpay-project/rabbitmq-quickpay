package flow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/celalsahinaltinisik/internal/amqpx"
	"github.com/celalsahinaltinisik/internal/forward"
	"github.com/celalsahinaltinisik/internal/model"
	"github.com/celalsahinaltinisik/internal/store"
	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	HeaderTraceID  = "x-trace-id"
	HeaderDeadline = "x-deadline" // unix milli
)

type Sender interface {
	Send(ctx context.Context, spec model.GroupSpec, body []byte,
		hdr http.Header, deadline time.Time) forward.Result
}

type Recorder interface {
	RecordAttempts(ctx context.Context, traceID string, attempts []store.AttemptInput) error
	FinishRequest(ctx context.Context, in store.FinishRequestInput) error
}

type Publisher func(ctx context.Context, replyTo string, pub amqp.Publishing) error

type Processor struct {
	sender  Sender
	rec     Recorder
	publish Publisher
	Now     func() time.Time
	Logf    func(string, ...any)
}

func NewProcessor(s Sender, r Recorder, p Publisher) *Processor {
	return &Processor{sender: s, rec: r, publish: p,
		Now: time.Now, Logf: func(string, ...any) {}}
}

// Handle จัดการข้อความหนึ่งใบจนจบ แล้ว ack เสมอ
//
// ไม่ requeue ในทุกกรณี เพราะ caller รอแบบ synchronous การ requeue
// จะสร้างออเดอร์ซ้ำให้คนที่เดินจากไปแล้ว ความล้มเหลวถูกบันทึกใน log แทน
func (p *Processor) Handle(ctx context.Context, d amqp.Delivery, spec model.GroupSpec) {
	start := p.Now()
	traceID := headerString(d.Headers, HeaderTraceID)

	defer func() {
		if r := recover(); r != nil {
			p.Logf("💥 panic ระหว่างประมวลผล trace=%s: %v", traceID, r)
			p.finish(ctx, traceID, model.StatusFailed, nil, 0, start, fmt.Sprintf("panic: %v", r))
			p.reply(ctx, d, errorBody(500, "internal error"), 500)
		}
		_ = d.Ack(false)
	}()

	deadline, ok := parseDeadline(d.Headers)
	if !ok || !p.Now().Before(deadline) {
		// ข้อความหมดอายุ — caller เลิกรอไปแล้ว ห้ามยิง upstream
		// นี่คือกลไกที่กันออเดอร์ผีแบบที่ระบบเก่ามีตอน consumer กลับมาหลัง deploy
		p.finish(ctx, traceID, model.StatusExpired, nil, 0, start, "ข้อความหมดอายุก่อนถูกประมวลผล")
		return
	}

	if !spec.HasUpstream() {
		p.finish(ctx, traceID, model.StatusNoUpstream, nil, 0, start, "group ไม่มี url ที่ active")
		p.reply(ctx, d, errorBody(503, "ไม่มีปลายทางที่พร้อมใช้งาน"), 503)
		return
	}

	res := p.sender.Send(ctx, spec, d.Body, amqpHeadersToHTTP(d.Headers), deadline)

	if len(res.Attempts) > 0 {
		if err := p.rec.RecordAttempts(ctx, traceID, toAttemptInputs(res.Attempts)); err != nil {
			p.Logf("⚠️  บันทึก attempt_logs ไม่สำเร็จ trace=%s: %v", traceID, err)
		}
	}

	if res.Final == nil {
		p.finish(ctx, traceID, model.StatusFailed, nil, len(res.Attempts), start, "ไม่ได้ยิง upstream เลย")
		p.reply(ctx, d, errorBody(504, "หมดเวลาก่อนได้ยิงปลายทาง"), 504)
		return
	}

	status := model.StatusFailed
	if res.Final.Outcome == model.OutcomeSuccess {
		status = model.StatusSuccess
	}
	p.finish(ctx, traceID, status, res.Final.Body, len(res.Attempts), start, res.Final.ErrMessage)

	body := res.Final.Body
	if len(body) == 0 {
		body = errorBody(res.Final.HTTPStatus, res.Final.ErrMessage)
	}
	upstream := res.Final.HTTPStatus
	if upstream == 0 {
		upstream = 502 // ยิงไม่ถึงปลายทางเลย
	}
	p.reply(ctx, d, body, upstream)
}

func (p *Processor) finish(ctx context.Context, traceID string, st model.RequestStatus,
	body []byte, attempts int, start time.Time, errMsg string) {
	if traceID == "" {
		return
	}
	err := p.rec.FinishRequest(ctx, store.FinishRequestInput{
		TraceID: traceID, Status: st, ResponseBody: body,
		AttemptCount: attempts,
		TotalMS:      int(p.Now().Sub(start) / time.Millisecond),
		ErrMessage:   errMsg,
	})
	if err != nil {
		p.Logf("⚠️  อัปเดต request_logs ไม่สำเร็จ trace=%s: %v", traceID, err)
	}
}

func (p *Processor) reply(ctx context.Context, d amqp.Delivery, body []byte, upstreamStatus int) {
	if d.ReplyTo == "" {
		return
	}
	err := p.publish(ctx, d.ReplyTo, amqp.Publishing{
		ContentType:   "application/json",
		CorrelationId: d.CorrelationId,
		Headers:       amqp.Table{amqpx.HeaderUpstreamStatus: int32(upstreamStatus)},
		Body:          body,
	})
	if err != nil {
		p.Logf("⚠️  ส่ง reply ไม่สำเร็จ corr=%s: %v", d.CorrelationId, err)
	}
}

func toAttemptInputs(as []forward.Attempt) []store.AttemptInput {
	out := make([]store.AttemptInput, 0, len(as))
	for _, a := range as {
		out = append(out, store.AttemptInput{
			Seq: a.Seq, URLID: a.URLID, URL: a.URL,
			HTTPStatus:   a.HTTPStatus,
			DurationMS:   int(a.Duration / time.Millisecond),
			Outcome:      a.Outcome,
			ResponseBody: string(a.Body),
			ErrMessage:   a.ErrMessage,
		})
	}
	return out
}

func parseDeadline(h amqp.Table) (time.Time, bool) {
	raw := headerString(h, HeaderDeadline)
	if raw == "" {
		return time.Time{}, false
	}
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.UnixMilli(ms), true
}

func headerString(h amqp.Table, key string) string {
	if h == nil {
		return ""
	}
	switch v := h[key].(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return ""
	}
}

// amqpHeadersToHTTP แปลง header ของ caller ที่ติดมากับข้อความกลับเป็น http.Header
// header ภายในของเราเอง (x-trace-id, x-deadline) ไม่ถูกส่งต่อไป upstream
func amqpHeadersToHTTP(h amqp.Table) http.Header {
	out := http.Header{}
	for k, v := range h {
		if k == HeaderTraceID || k == HeaderDeadline {
			continue
		}
		if s, ok := v.(string); ok {
			out.Add(k, s)
		}
	}
	return out
}

func errorBody(code int, msg string) []byte {
	b, err := json.Marshal(map[string]any{"code": code, "message": msg})
	if err != nil {
		return []byte(`{"code":500,"message":"internal error"}`)
	}
	return b
}
