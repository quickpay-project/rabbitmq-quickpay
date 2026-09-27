package forward

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/celalsahinaltinisik/internal/model"
)

// newTestForwarder ปิดการสุ่มเพื่อให้ลำดับ url แน่นอนในการทดสอบ
func newTestForwarder() *Forwarder {
	f := New()
	f.Shuffle = func([]model.URLSpec) {}
	return f
}

func specWith(urls ...string) model.GroupSpec {
	g := model.GroupSpec{
		Name:            "withdraw",
		UpstreamTimeout: 2 * time.Second,
		RPCTimeout:      10 * time.Second,
	}
	for i, u := range urls {
		g.URLs = append(g.URLs, model.URLSpec{ID: int64(i + 1), URL: u})
	}
	return g
}

func farDeadline() time.Time { return time.Now().Add(time.Minute) }

func TestSendStopsAtFirstSuccess(t *testing.T) {
	hits := 0
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer ok.Close()

	res := newTestForwarder().Send(context.Background(),
		specWith(ok.URL, ok.URL), []byte(`{}`), http.Header{}, farDeadline())

	if len(res.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(res.Attempts))
	}
	if hits != 1 {
		t.Errorf("ยิง upstream %d ครั้ง, want 1", hits)
	}
	if res.Final == nil || res.Final.Outcome != model.OutcomeSuccess {
		t.Fatalf("Final = %+v, want success", res.Final)
	}
	if string(res.Final.Body) != `{"code":0}` {
		t.Errorf("Body = %q", res.Final.Body)
	}
}

func TestSendFailsOverOnRetryableThenSucceeds(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer good.Close()

	res := newTestForwarder().Send(context.Background(),
		specWith(bad.URL, good.URL), []byte(`{}`), http.Header{}, farDeadline())

	if len(res.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(res.Attempts))
	}
	if res.Attempts[0].Seq != 1 || res.Attempts[1].Seq != 2 {
		t.Errorf("seq ต้องเป็น 1,2 แต่ได้ %d,%d", res.Attempts[0].Seq, res.Attempts[1].Seq)
	}
	if res.Attempts[0].Outcome != model.OutcomeRetryable {
		t.Errorf("attempt 1 = %q, want retryable", res.Attempts[0].Outcome)
	}
	if res.Final.Outcome != model.OutcomeSuccess {
		t.Errorf("Final = %q, want success", res.Final.Outcome)
	}
}

func TestSendStopsOnFatalWithoutTryingNextURL(t *testing.T) {
	secondHit := false
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"message":"ยอดเงินไม่พอ"}`))
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHit = true
		w.WriteHeader(200)
	}))
	defer second.Close()

	res := newTestForwarder().Send(context.Background(),
		specWith(first.URL, second.URL), []byte(`{}`), http.Header{}, farDeadline())

	if len(res.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(res.Attempts))
	}
	if secondHit {
		t.Fatal("400 คือคำขอผิดเอง ห้ามลอง url ถัดไป")
	}
	if res.Final.Outcome != model.OutcomeFatal {
		t.Errorf("Final = %q, want fatal", res.Final.Outcome)
	}
}

func TestTimeoutAfterRequestSentIsFatalNotRetried(t *testing.T) {
	secondHit := false
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer slow.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHit = true
	}))
	defer second.Close()

	spec := specWith(slow.URL, second.URL)
	spec.UpstreamTimeout = 100 * time.Millisecond // สั้นกว่าที่ server ใช้ตอบ

	res := newTestForwarder().Send(context.Background(),
		spec, []byte(`{}`), http.Header{}, farDeadline())

	if secondHit {
		t.Fatal("timeout หลังส่ง body แล้ว ห้ามลอง url ถัดไป เพราะออเดอร์อาจเกิดแล้ว")
	}
	if res.Final.Outcome != model.OutcomeFatal {
		t.Fatalf("Final = %q, want fatal", res.Final.Outcome)
	}
}

func TestConnectionRefusedIsRetryable(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // ปิดทิ้งเพื่อให้ต่อไม่ติด

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer good.Close()

	res := newTestForwarder().Send(context.Background(),
		specWith(deadURL, good.URL), []byte(`{}`), http.Header{}, farDeadline())

	if len(res.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2 (ต่อไม่ติดต้องลองตัวถัดไป)", len(res.Attempts))
	}
	if res.Attempts[0].Outcome != model.OutcomeRetryable {
		t.Errorf("attempt 1 = %q, want retryable", res.Attempts[0].Outcome)
	}
	if res.Final.Outcome != model.OutcomeSuccess {
		t.Errorf("Final = %q, want success", res.Final.Outcome)
	}
}

// Review Focus #4 — url ผิดรูปใน DB ต้องไม่ทำให้ panic
func TestMalformedURLIsFatalAttemptNotPanic(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer good.Close()

	for _, bad := range []string{"", "   ", "not-a-url", "http://[::1]:namedport"} {
		res := newTestForwarder().Send(context.Background(),
			specWith(bad), []byte(`{}`), http.Header{}, farDeadline())
		if len(res.Attempts) != 1 {
			t.Fatalf("url %q: attempts = %d, want 1", bad, len(res.Attempts))
		}
		if res.Attempts[0].Outcome != model.OutcomeFatal {
			t.Errorf("url %q: outcome = %q, want fatal", bad, res.Attempts[0].Outcome)
		}
		if res.Attempts[0].ErrMessage == "" {
			t.Errorf("url %q: ต้องมี ErrMessage ไว้ให้ไล่ปัญหา", bad)
		}
	}
}

func TestNoActiveURLReturnsEmptyResult(t *testing.T) {
	res := newTestForwarder().Send(context.Background(),
		specWith(), []byte(`{}`), http.Header{}, farDeadline())
	if len(res.Attempts) != 0 || res.Final != nil {
		t.Fatalf("group ที่ไม่มี url ต้องได้ผลว่าง แต่ได้ %+v", res)
	}
}

func TestDeadlineAlreadyPassedMeansNoAttempt(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
	}))
	defer srv.Close()

	res := newTestForwarder().Send(context.Background(),
		specWith(srv.URL), []byte(`{}`), http.Header{}, time.Now().Add(-time.Second))

	if hit {
		t.Fatal("deadline ผ่านไปแล้ว ห้ามยิง upstream")
	}
	if len(res.Attempts) != 0 {
		t.Fatalf("attempts = %d, want 0", len(res.Attempts))
	}
}

func TestStopsWhenRemainingBudgetTooSmallForAnotherAttempt(t *testing.T) {
	secondHit := false
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHit = true
	}))
	defer second.Close()

	f := newTestForwarder()
	f.MinAttemptBudget = time.Hour // ทำให้ไม่เหลือ budget พอสำหรับครั้งที่สองแน่ ๆ

	res := f.Send(context.Background(),
		specWith(first.URL, second.URL), []byte(`{}`), http.Header{}, time.Now().Add(2*time.Second))

	if secondHit {
		t.Fatal("เวลาไม่พอแล้ว ห้ามเริ่ม attempt ใหม่")
	}
	if len(res.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(res.Attempts))
	}
}

func TestForwardsCallerHeadersButStripsHopByHop(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		got.Set("Host", r.Host)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer abc123")
	hdr.Set("X-Merchant-Id", "M001")
	hdr.Set("Connection", "keep-alive")
	hdr.Set("Host", "evil.example.com")

	newTestForwarder().Send(context.Background(),
		specWith(srv.URL), []byte(`{}`), hdr, farDeadline())

	if got.Get("Authorization") != "Bearer abc123" {
		t.Error("Authorization ต้องถูกส่งต่อ")
	}
	if got.Get("X-Merchant-Id") != "M001" {
		t.Error("header ของ caller ตัวอื่นต้องถูกส่งต่อ")
	}
	if got.Get("Connection") == "keep-alive" {
		t.Error("Connection เป็น hop-by-hop ต้องไม่ถูกส่งต่อ")
	}
	if got.Get("Host") == "evil.example.com" {
		t.Error("Host ของ caller ต้องไม่ถูกส่งต่อไป upstream")
	}
	if got.Get("Content-Type") != "application/json" {
		t.Error("ต้องตั้ง Content-Type เป็น application/json")
	}
}

func TestOversizedResponseIsFatalNotTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		blob := make([]byte, 4096)
		_, _ = w.Write(blob)
	}))
	defer srv.Close()

	f := newTestForwarder()
	f.MaxResponseBytes = 1024

	res := f.Send(context.Background(),
		specWith(srv.URL), []byte(`{}`), http.Header{}, farDeadline())

	if res.Final.Outcome != model.OutcomeFatal {
		t.Fatalf("Final = %q, want fatal — response ใหญ่เกินต้องไม่ถูกตัดแล้วส่งต่อเงียบ ๆ",
			res.Final.Outcome)
	}
}

func TestAttemptRecordsURLIDAndDuration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	res := newTestForwarder().Send(context.Background(),
		specWith(srv.URL), []byte(`{}`), http.Header{}, farDeadline())

	a := res.Attempts[0]
	if a.URLID != 1 {
		t.Errorf("URLID = %d, want 1 — ต้องผูกกลับไปหาแถวใน message_group_url ได้", a.URLID)
	}
	if a.URL != srv.URL {
		t.Errorf("URL = %q, want %q", a.URL, srv.URL)
	}
	if a.Duration <= 0 {
		t.Error("Duration ต้องถูกบันทึก")
	}
}
