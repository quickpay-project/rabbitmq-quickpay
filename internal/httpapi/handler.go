package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/celalsahinaltinisik/internal/amqpx"
	"github.com/celalsahinaltinisik/internal/config"
	"github.com/celalsahinaltinisik/internal/flow"
	"github.com/celalsahinaltinisik/internal/model"
	"github.com/celalsahinaltinisik/internal/store"
	amqp "github.com/rabbitmq/amqp091-go"
)

const maxRequestBytes = 4 * 1024 * 1024

type RequestLogger interface {
	BeginRequest(ctx context.Context, in store.BeginRequestInput) error
	MarkClientOutcome(ctx context.Context, traceID string, httpStatus int) error
}

// BlockedRecorder รับได้เสมอและต้องไม่บล็อก — อยู่ในเส้นทางของทุกคำขอที่ถูกปฏิเสธ
type BlockedRecorder interface {
	Record(in store.BlockedInput)
}

type RPCCaller interface {
	Call(ctx context.Context, queue string, pub amqp.Publishing) (*amqp.Delivery, error)
}

type Options struct {
	Registry *flow.Registry
	Logger   RequestLogger
	Caller   RPCCaller
	Cfg      *config.Config
	NewID    func() string
	Now      func() time.Time
	Logf     func(string, ...any)

	// Blocked นับ request ที่ถูกปฏิเสธที่ชั้น allowlist ลง DB — nil ได้ (ข้ามไป)
	// จำเป็นเพราะการปฏิเสธเกิดก่อน BeginRequest คำขอพวกนี้จึงไม่มีร่องรอยใน request_logs
	// ทำให้ตอบไม่ได้ว่าใครถูกบล็อกไปเท่าไหร่ ซึ่งเจ็บจริงตอน cutover 2026-09-28
	Blocked BlockedRecorder

	// AMQPHealthy บอกว่า connection ของ broker ยังใช้ได้ไหม (amqpx.Manager.Healthy)
	// เป็น func เพื่อไม่ให้ httpapi ต้องรู้จัก amqpx และเพื่อให้ fake ได้ใน test
	// สถานะ flow อย่างเดียวไม่พอ เพราะ consumer กู้ตัวเองได้แต่ RPC pool ไม่กู้
	// readyz จึงเขียวได้ทั้งที่เส้นทาง request ตายสนิท
	AMQPHealthy func() bool
}

type Handler struct{ o Options }

func New(o Options) *Handler {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.AMQPHealthy == nil {
		// ไม่ได้ผูกไว้ = ไม่มีข้อมูล ไม่ตัดสินว่าพัง (cmd/gateway ผูก mgr.Healthy ให้จริง)
		o.AMQPHealthy = func() bool { return true }
	}
	return &Handler{o: o}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	case "/readyz":
		h.readyz(w)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ip := ClientIP(r, h.o.Cfg.TrustedProxyCount, h.o.Cfg.ClientIPHeader)
	if !IsAllowed(ip, h.o.Cfg.AllowedIPs, h.o.Cfg.AllowAllIPs) {
		// บอกให้ชัดว่าปฏิเสธเพราะหา IP ไม่ได้ ไม่ใช่เพราะ IP ไม่อยู่ในรายการ
		// สองกรณีนี้แก้คนละทางและแยกไม่ออกจาก log ที่เขียนว่า "ปฏิเสธ IP " เฉย ๆ
		reason := store.BlockedNotInAllowlist
		if ip == "" && h.o.Cfg.ClientIPHeader != "" {
			reason = store.BlockedMissingIPHeader
			h.o.Logf("🚫 ไม่มี header %s ในคำขอที่ %s — คำขอไม่ได้ผ่าน proxy ที่ประกาศว่าเชื่อถือ",
				h.o.Cfg.ClientIPHeader, r.URL.Path)
		} else {
			h.o.Logf("🚫 ปฏิเสธ IP %s ที่ %s", ip, r.URL.Path)
		}
		if h.o.Blocked != nil {
			h.o.Blocked.Record(store.BlockedInput{ClientIP: ip, Path: r.URL.Path,
				Reason: reason, Chain: chainSnapshot(r)})
		}
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	name := strings.Trim(r.URL.Path, "/")
	if name == "" || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}

	f, state, ok := h.o.Registry.ByName(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	spec := f.Spec()

	if state != flow.StateRunning || !spec.HasUpstream() {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "flow ยังไม่พร้อมรับงาน: "+string(state), http.StatusServiceUnavailable)
		return
	}

	// อ่านเกินลิมิตไป 1 ไบต์เพื่อ "ตรวจจับ" ว่าเกิน ไม่ใช่เพื่อใช้งาน
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		http.Error(w, "อ่าน body ไม่สำเร็จ", http.StatusBadRequest)
		return
	}
	if len(body) > maxRequestBytes {
		// ปฏิเสธ ไม่ตัดแล้วใช้ต่อ — payload ที่ถูกตัดคือ JSON พังที่จะถูกเขียนลง
		// request_logs, ถูก ExtractRef ดึง business_ref ผิด และถูกยิง upstream
		// ราวกับเป็นของสมบูรณ์ ซึ่งเป็นปัญหา data integrity ไม่ใช่เรื่องประสิทธิภาพ
		// รูปแบบเดียวกับที่ internal/forward ทำกับ response body ที่ใหญ่เกิน
		h.o.Logf("🚫 body ใหญ่เกิน %d ไบต์ ที่ %s จาก %s", maxRequestBytes, r.URL.Path, ip)
		http.Error(w, "body ใหญ่เกินที่รับได้", http.StatusRequestEntityTooLarge)
		return
	}

	traceID := h.o.NewID()
	w.Header().Set("X-Trace-Id", traceID)

	start := h.o.Now()
	deadline := start.Add(spec.RPCTimeout)

	beginErr := h.o.Logger.BeginRequest(r.Context(), store.BeginRequestInput{
		TraceID: traceID, GroupID: spec.ID, GroupName: spec.Name,
		CallerTraceID: r.Header.Get("X-Trace-Id"),
		ClientIP:      ip,
		Body:          body,
		BusinessRef:   model.ExtractRef(body, spec.RefField),
	})
	if beginErr != nil {
		// ถ้าบันทึกไม่ได้ก็ไม่ควรทำงานต่อ เพราะจะกลายเป็นออเดอร์ที่ไม่มีร่องรอย
		h.o.Logf("❌ BeginRequest ล้มเหลว trace=%s: %v", traceID, beginErr)
		http.Error(w, "ระบบบันทึกไม่พร้อม", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()

	headers := amqp.Table{
		flow.HeaderTraceID:  traceID,
		flow.HeaderDeadline: strconv.FormatInt(deadline.UnixMilli(), 10),
	}
	for k, vs := range r.Header {
		if len(vs) > 0 && !hopByHopRequest[http.CanonicalHeaderKey(k)] {
			headers[k] = vs[0]
		}
	}

	reply, err := h.o.Caller.Call(ctx, spec.QueueName(h.o.Cfg.QueuePrefix), amqp.Publishing{
		ContentType:   "application/json",
		CorrelationId: traceID,
		Headers:       headers,
		Body:          body,
	})
	if err != nil {
		h.o.Logf("⏱  ไม่ได้รับ reply trace=%s: %v", traceID, err)
		h.mark(r.Context(), traceID, http.StatusGatewayTimeout)
		http.Error(w, "upstream ไม่ตอบกลับในเวลาที่กำหนด", http.StatusGatewayTimeout)
		return
	}

	status := upstreamStatus(reply)
	h.mark(r.Context(), traceID, status)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(reply.Body)
}

func (h *Handler) mark(ctx context.Context, traceID string, status int) {
	if err := h.o.Logger.MarkClientOutcome(ctx, traceID, status); err != nil {
		h.o.Logf("⚠️  บันทึกผลฝั่ง caller ไม่สำเร็จ trace=%s: %v", traceID, err)
	}
}

func (h *Handler) readyz(w http.ResponseWriter) {
	flowsReady, states := h.o.Registry.AllRunning()
	out := map[string]string{}
	for name, st := range states {
		out[name] = string(st)
	}

	// สุขภาพ AMQP เป็นเงื่อนไขแยก: flow ทุกตัวอาจ running แต่ถ้า connection หลุด
	// ทุก request จะได้ 504 ถาวร ต้องตอบ not-ready เพื่อให้ orchestrator restart pod
	amqpUp := h.o.AMQPHealthy()
	amqpStatus := "up"
	if !amqpUp {
		amqpStatus = "down"
	}
	ready := flowsReady && amqpUp

	w.Header().Set("Content-Type", "application/json")
	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ready": ready, "flows": out, "amqp": amqpStatus,
	})
}

func upstreamStatus(d *amqp.Delivery) int {
	if d == nil || d.Headers == nil {
		return http.StatusOK
	}
	var raw int
	switch v := d.Headers[amqpx.HeaderUpstreamStatus].(type) {
	case int32:
		raw = int(v)
	case int64:
		raw = int(v)
	case int:
		raw = v
	case string:
		n, err := strconv.Atoi(v)
		if err != nil {
			return http.StatusOK
		}
		raw = n
	default:
		return http.StatusOK
	}
	// ค่านอกช่วงที่ WriteHeader ยอมรับจะทำให้ net/http panic ทันที
	// clamp เป็น 502 เพื่อไม่ต้องพึ่งสมมติฐานว่าฝั่งที่ publish reply ส่งค่าถูกเสมอ
	if raw < 100 || raw > 999 {
		return http.StatusBadGateway
	}
	return raw
}

var hopByHopRequest = map[string]bool{
	"Connection":        true,
	"Keep-Alive":        true,
	"Transfer-Encoding": true,
	"Upgrade":           true,
	"Host":              true,
	"Content-Length":    true,
}
