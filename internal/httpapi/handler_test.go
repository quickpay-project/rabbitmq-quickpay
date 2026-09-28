package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/celalsahinaltinisik/internal/amqpx"
	"github.com/celalsahinaltinisik/internal/config"
	"github.com/celalsahinaltinisik/internal/flow"
	"github.com/celalsahinaltinisik/internal/model"
	"github.com/celalsahinaltinisik/internal/store"
	amqp "github.com/rabbitmq/amqp091-go"
)

type fakeLogger struct {
	begun    []store.BeginRequestInput
	outcomes map[string]int
}

func newFakeLogger() *fakeLogger { return &fakeLogger{outcomes: map[string]int{}} }

func (f *fakeLogger) BeginRequest(_ context.Context, in store.BeginRequestInput) error {
	f.begun = append(f.begun, in)
	return nil
}

func (f *fakeLogger) MarkClientOutcome(_ context.Context, traceID string, status int) error {
	f.outcomes[traceID] = status
	return nil
}

type fakeCaller struct {
	reply *amqp.Delivery
	err   error
	queue string
	pub   amqp.Publishing
}

func (f *fakeCaller) Call(_ context.Context, queue string, pub amqp.Publishing) (*amqp.Delivery, error) {
	f.queue, f.pub = queue, pub
	return f.reply, f.err
}

type stubBroker struct{ msgs chan amqp.Delivery }

func (b *stubBroker) DeclareQueue(string) error { return nil }
func (b *stubBroker) Consume(string, int) (<-chan amqp.Delivery, string, error) {
	return b.msgs, "t", nil
}
func (b *stubBroker) Cancel(string) error { return nil }
func (b *stubBroker) Close() error        { return nil }

func registryWith(name string, st flow.State, urls ...string) *flow.Registry {
	spec := model.GroupSpec{ID: "g1", Name: name, WorkerCount: 2,
		RPCTimeout: 30 * time.Second, RefField: "customer_order_id"}
	for i, u := range urls {
		spec.URLs = append(spec.URLs, model.URLSpec{ID: int64(i + 1), URL: u})
	}
	f := flow.New(flow.Options{Spec: spec, Queue: "v2." + name,
		Broker:  &stubBroker{msgs: make(chan amqp.Delivery)},
		Process: func(context.Context, amqp.Delivery, model.GroupSpec) {}})
	r := flow.NewRegistry()
	r.Put("g1", f, st)
	return r
}

func testHandler(reg *flow.Registry, lg *fakeLogger, caller *fakeCaller) *Handler {
	return New(Options{
		Registry: reg,
		Logger:   lg,
		Caller:   caller,
		Cfg: &config.Config{QueuePrefix: "v2.", AllowAllIPs: true,
			TrustedProxyCount: 0},
		NewID: func() string { return "trace-fixed" },
	})
}

func post(h *Handler, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.RemoteAddr = "203.0.113.9:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func okReply(status int, body string) *amqp.Delivery {
	return &amqp.Delivery{
		Headers: amqp.Table{amqpx.HeaderUpstreamStatus: int32(status)},
		Body:    []byte(body),
	}
}

func TestPostForwardsAndReturnsTraceHeader(t *testing.T) {
	lg, caller := newFakeLogger(), &fakeCaller{reply: okReply(200, `{"code":0}`)}
	h := testHandler(registryWith("withdraw", flow.StateRunning, "https://a"), lg, caller)

	w := post(h, "/withdraw", `{"customer_order_id":"ORDER-1"}`)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != `{"code":0}` {
		t.Errorf("body = %q — ต้องเป็น passthrough", w.Body.String())
	}
	if w.Header().Get("X-Trace-Id") != "trace-fixed" {
		t.Error("ต้องคืน X-Trace-Id ให้ support ไล่ปัญหาจากที่ลูกค้าแจ้งได้")
	}
	if caller.queue != "v2.withdraw" {
		t.Errorf("queue = %q, want v2.withdraw — prefix ต้องถูกใส่", caller.queue)
	}
	if len(lg.begun) != 1 || lg.begun[0].BusinessRef != "ORDER-1" {
		t.Errorf("BeginRequest = %+v — ต้องดึง business_ref จาก body", lg.begun)
	}
	if lg.outcomes["trace-fixed"] != 200 {
		t.Errorf("MarkClientOutcome = %d, want 200", lg.outcomes["trace-fixed"])
	}
}

func TestUpstreamStatusIsPassedThrough(t *testing.T) {
	lg, caller := newFakeLogger(), &fakeCaller{reply: okReply(400, `{"message":"ยอดเงินไม่พอ"}`)}
	h := testHandler(registryWith("withdraw", flow.StateRunning, "https://a"), lg, caller)

	w := post(h, "/withdraw", `{}`)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400 ตาม spec §7.7", w.Code)
	}
	if lg.outcomes["trace-fixed"] != 400 {
		t.Errorf("http_status ที่บันทึก = %d, want 400", lg.outcomes["trace-fixed"])
	}
}

func TestPublishesTraceAndDeadlineHeaders(t *testing.T) {
	lg, caller := newFakeLogger(), &fakeCaller{reply: okReply(200, `{}`)}
	h := testHandler(registryWith("withdraw", flow.StateRunning, "https://a"), lg, caller)

	post(h, "/withdraw", `{}`)

	if caller.pub.Headers[flow.HeaderTraceID] != "trace-fixed" {
		t.Error("ต้องส่ง x-trace-id ไปกับข้อความ")
	}
	if caller.pub.Headers[flow.HeaderDeadline] == nil {
		t.Fatal("ต้องส่ง x-deadline ไปกับข้อความ ไม่งั้น worker จะยิง upstream หลัง caller เลิกรอ")
	}
	if caller.pub.CorrelationId == "" {
		t.Error("ต้องมี correlation_id")
	}
}

func TestRPCTimeoutReturns504AndRecordsIt(t *testing.T) {
	lg := newFakeLogger()
	caller := &fakeCaller{err: context.DeadlineExceeded}
	h := testHandler(registryWith("withdraw", flow.StateRunning, "https://a"), lg, caller)

	w := post(h, "/withdraw", `{}`)
	if w.Code != 504 {
		t.Fatalf("status = %d, want 504", w.Code)
	}
	if lg.outcomes["trace-fixed"] != 504 {
		t.Errorf("ต้องบันทึกว่า caller ได้ 504 ไว้ให้ไล่เคส 'caller คิดว่าล้มแต่ออเดอร์เกิดจริง'")
	}
}

func TestUnknownGroupIs404(t *testing.T) {
	h := testHandler(registryWith("withdraw", flow.StateRunning, "https://a"),
		newFakeLogger(), &fakeCaller{reply: okReply(200, `{}`)})
	if w := post(h, "/ไม่มีกลุ่มนี้", `{}`); w.Code != 404 {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestDrainingGroupIs503(t *testing.T) {
	h := testHandler(registryWith("withdraw", flow.StateDraining, "https://a"),
		newFakeLogger(), &fakeCaller{reply: okReply(200, `{}`)})
	w := post(h, "/withdraw", `{}`)
	if w.Code != 503 {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("503 ควรมี Retry-After")
	}
}

func TestGroupWithoutUpstreamIs503(t *testing.T) {
	h := testHandler(registryWith("withdraw", flow.StateDegraded),
		newFakeLogger(), &fakeCaller{reply: okReply(200, `{}`)})
	if w := post(h, "/withdraw", `{}`); w.Code != 503 {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestNonPostIs405(t *testing.T) {
	h := testHandler(registryWith("withdraw", flow.StateRunning, "https://a"),
		newFakeLogger(), &fakeCaller{reply: okReply(200, `{}`)})
	r := httptest.NewRequest(http.MethodGet, "/withdraw", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 405 {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

func TestBlockedIPIs403AndNeverPublishes(t *testing.T) {
	lg, caller := newFakeLogger(), &fakeCaller{reply: okReply(200, `{}`)}
	h := New(Options{
		Registry: registryWith("withdraw", flow.StateRunning, "https://a"),
		Logger:   lg, Caller: caller,
		Cfg:   &config.Config{QueuePrefix: "v2.", AllowedIPs: []string{"198.51.100.1"}},
		NewID: func() string { return "trace-fixed" },
	})

	w := post(h, "/withdraw", `{}`)
	if w.Code != 403 {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if caller.queue != "" {
		t.Error("IP ไม่ผ่านต้องไม่ publish อะไรเลย")
	}
	if len(lg.begun) != 0 {
		t.Error("IP ไม่ผ่านต้องไม่สร้างแถวใน request_logs")
	}
}

func TestHealthzAlwaysOK(t *testing.T) {
	h := testHandler(registryWith("withdraw", flow.StateFailed, "https://a"),
		newFakeLogger(), &fakeCaller{})
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("healthz = %d, want 200 — ต้องตอบได้แม้ flow ยังไม่ขึ้น", w.Code)
	}
}

func TestReadyzReflectsFlowState(t *testing.T) {
	h := testHandler(registryWith("withdraw", flow.StateFailed, "https://a"),
		newFakeLogger(), &fakeCaller{})
	r := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != 503 {
		t.Fatalf("readyz = %d, want 503 เมื่อมี flow ที่ failed", w.Code)
	}
	var body struct {
		Ready bool              `json:"ready"`
		Flows map[string]string `json:"flows"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("อ่าน body ไม่ได้: %v", err)
	}
	if body.Ready || body.Flows["withdraw"] != "failed" {
		t.Errorf("body = %+v — ต้องบอกเป็นรายตัวว่าใครไม่ขึ้น", body)
	}
}

// Fix review finding #1 — body ที่ใหญ่เกินต้องถูกปฏิเสธ ไม่ใช่ถูกตัดแล้วบันทึก/ส่งต่อ
// payload ที่ถูกตัดคือ JSON พังที่ถูกเขียนลง request_logs และถูกยิง upstream
// ราวกับเป็นของสมบูรณ์ — เป็นปัญหา data integrity ของงานการเงิน
func TestOversizedBodyIs413AndNeverPublishes(t *testing.T) {
	lg, caller := newFakeLogger(), &fakeCaller{reply: okReply(200, `{}`)}
	h := testHandler(registryWith("withdraw", flow.StateRunning, "https://a"), lg, caller)

	// เกินลิมิตไป 1 ไบต์พอดี
	pad := strings.Repeat("a", maxRequestBytes-9)
	w := post(h, "/withdraw", `{"pad":"`+pad+`"}`)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", w.Code)
	}
	if caller.queue != "" {
		t.Error("body เกินขนาดต้องไม่ publish อะไรเลย")
	}
	if len(lg.begun) != 0 {
		t.Error("body เกินขนาดต้องไม่สร้างแถวใน request_logs")
	}
	if len(lg.outcomes) != 0 {
		t.Error("body เกินขนาดต้องไม่บันทึกผลฝั่ง caller")
	}
}

// ขอบเขตพอดีต้องผ่าน — กันการแก้แบบ off-by-one ที่ไปปฏิเสธของที่ยังรับได้
func TestBodyExactlyAtLimitStillPasses(t *testing.T) {
	lg, caller := newFakeLogger(), &fakeCaller{reply: okReply(200, `{"code":0}`)}
	h := testHandler(registryWith("withdraw", flow.StateRunning, "https://a"), lg, caller)

	pad := strings.Repeat("a", maxRequestBytes-10) // `{"pad":"` + pad + `"}` = maxRequestBytes พอดี
	body := `{"pad":"` + pad + `"}`
	if len(body) != maxRequestBytes {
		t.Fatalf("เตรียม body ผิด: %d ไบต์, want %d", len(body), maxRequestBytes)
	}

	w := post(h, "/withdraw", body)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 — ขนาดพอดีลิมิตต้องยังรับได้", w.Code)
	}
	if len(lg.begun) != 1 {
		t.Error("ต้องบันทึก request_logs ตามปกติ")
	}
}

// Fix review finding #2 — ค่า x-upstream-status นอกช่วง 100-999 ทำให้ WriteHeader panic
func TestOutOfRangeUpstreamStatusBecomes502(t *testing.T) {
	for _, bad := range []int{0, 99, 1000, -5} {
		lg, caller := newFakeLogger(), &fakeCaller{reply: okReply(bad, `{"x":1}`)}
		h := testHandler(registryWith("withdraw", flow.StateRunning, "https://a"), lg, caller)

		w := post(h, "/withdraw", `{}`)
		if w.Code != http.StatusBadGateway {
			t.Errorf("x-upstream-status %d: status = %d, want 502", bad, w.Code)
		}
		if lg.outcomes["trace-fixed"] != http.StatusBadGateway {
			t.Errorf("x-upstream-status %d: บันทึก = %d, want 502",
				bad, lg.outcomes["trace-fixed"])
		}
	}
}

type fakeBlocked struct{ got []store.BlockedInput }

func (f *fakeBlocked) Record(in store.BlockedInput) { f.got = append(f.got, in) }

func denyingHandler(fb BlockedRecorder, ipHeader string) *Handler {
	return New(Options{
		Registry: registryWith("deposit", flow.StateRunning, "https://u"),
		Logger:   newFakeLogger(),
		Caller:   &fakeCaller{},
		Cfg: &config.Config{QueuePrefix: "v2.", AllowedIPs: []string{"9.9.9.9"},
			AllowAllIPs: false, TrustedProxyCount: 0, ClientIPHeader: ipHeader},
		NewID:   func() string { return "trace-fixed" },
		Blocked: fb,
	})
}

func postFrom(h *Handler, remote string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/deposit", strings.NewReader("{}"))
	r.RemoteAddr = remote
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// IP ที่ไม่อยู่ในรายการต้องถูกนับ ไม่ใช่หายไปเงียบ ๆ ใน log ของ container ที่หายตอน restart
// เป็นข้อมูลชิ้นเดียวที่ตอบได้ว่าใครถูกบล็อกไปเท่าไหร่ — เจ็บจริงตอน cutover 2026-09-28
func TestForbiddenIsRecorded(t *testing.T) {
	fb := &fakeBlocked{}
	if w := postFrom(denyingHandler(fb, ""), "1.2.3.4:5555"); w.Code != http.StatusForbidden {
		t.Fatalf("อยากได้ 403 แต่ได้ %d", w.Code)
	}
	if len(fb.got) != 1 {
		t.Fatalf("อยากได้ 1 รายการ แต่ได้ %d", len(fb.got))
	}
	if fb.got[0].ClientIP != "1.2.3.4" || fb.got[0].Path != "/deposit" ||
		fb.got[0].Reason != store.BlockedNotInAllowlist {
		t.Fatalf("ได้ %+v", fb.got[0])
	}
}

// ตั้ง CLIENT_IP_HEADER แล้วคำขอไม่มี header นั้น ต้องแยกเหตุผลออกจากกรณี IP ไม่อยู่ในรายการ
// เพราะสองอย่างนี้แก้คนละทาง — อันหนึ่งแก้ที่รายการ IP อีกอันแก้ที่เส้นทาง proxy
func TestForbiddenRecordsMissingHeaderReason(t *testing.T) {
	fb := &fakeBlocked{}
	if w := postFrom(denyingHandler(fb, "CF-Connecting-IP"), "1.2.3.4:5555"); w.Code != http.StatusForbidden {
		t.Fatalf("อยากได้ 403 แต่ได้ %d", w.Code)
	}
	if len(fb.got) != 1 || fb.got[0].Reason != store.BlockedMissingIPHeader {
		t.Fatalf("ได้ %+v", fb.got)
	}
}

// Blocked เป็น nil ได้ ต้องไม่ panic
func TestForbiddenWithoutRecorderDoesNotPanic(t *testing.T) {
	if w := postFrom(denyingHandler(nil, ""), "1.2.3.4:5555"); w.Code != http.StatusForbidden {
		t.Fatalf("อยากได้ 403 แต่ได้ %d", w.Code)
	}
}
