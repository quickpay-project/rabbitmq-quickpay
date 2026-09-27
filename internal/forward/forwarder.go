package forward

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"

	"github.com/celalsahinaltinisik/internal/model"
)

// Attempt คือบันทึกของการยิง upstream หนึ่งครั้ง ตรงกับหนึ่งแถวใน attempt_logs
type Attempt struct {
	Seq        int
	URLID      int64
	URL        string
	HTTPStatus int
	Duration   time.Duration
	Outcome    model.Outcome
	Body       []byte
	ErrMessage string
}

// Result คือผลรวมของการพยายามทั้งหมดสำหรับหนึ่ง request
// Final เป็น nil เมื่อไม่มี url ให้ยิงเลย (caller ต้องแปลเป็น no_upstream)
type Result struct {
	Attempts []Attempt
	Final    *Attempt
}

type Forwarder struct {
	Client           *http.Client
	Shuffle          func([]model.URLSpec)
	Now              func() time.Time
	MinAttemptBudget time.Duration
	MaxResponseBytes int64
}

func New() *Forwarder {
	return &Forwarder{
		// ไม่ตั้ง Timeout ที่ Client เพราะคุมด้วย context ต่อ attempt แทน
		Client:           &http.Client{},
		Shuffle:          func(u []model.URLSpec) { rand.Shuffle(len(u), func(i, j int) { u[i], u[j] = u[j], u[i] }) },
		Now:              time.Now,
		MinAttemptBudget: time.Second,
		MaxResponseBytes: 8 * 1024 * 1024,
	}
}

var hopByHop = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Host":                true,
	"Content-Length":      true,
}

// Send ยิง upstream ทีละ url จนกว่าจะสำเร็จ เจอ fatal หรือหมดเวลา
// deadline มาจาก header x-deadline ของข้อความ ซึ่งคำนวณตอนรับ HTTP เข้ามา
// ทำให้ worker ไม่มีทางยิง upstream หลังจาก caller เลิกรอไปแล้ว
func (f *Forwarder) Send(ctx context.Context, spec model.GroupSpec, body []byte,
	hdr http.Header, deadline time.Time) Result {

	urls := make([]model.URLSpec, len(spec.URLs))
	copy(urls, spec.URLs)
	f.Shuffle(urls)

	res := Result{}
	for i, u := range urls {
		remaining := deadline.Sub(f.Now())
		if remaining <= 0 {
			break
		}
		// ไม่เริ่ม attempt ใหม่ที่เวลาเหลือน้อยจนไม่น่าจะทัน
		if i > 0 && remaining < f.MinAttemptBudget {
			break
		}
		timeout := spec.UpstreamTimeout
		if timeout > remaining {
			timeout = remaining
		}

		a := f.attempt(ctx, i+1, u, body, hdr, timeout)
		res.Attempts = append(res.Attempts, a)
		res.Final = &res.Attempts[len(res.Attempts)-1]

		if a.Outcome != model.OutcomeRetryable {
			break
		}
	}
	return res
}

func (f *Forwarder) attempt(ctx context.Context, seq int, u model.URLSpec, body []byte,
	hdr http.Header, timeout time.Duration) Attempt {

	a := Attempt{Seq: seq, URLID: u.ID, URL: u.URL}
	start := f.Now()

	rawURL := strings.TrimSpace(u.URL)
	if rawURL == "" || !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		a.Outcome = model.OutcomeFatal
		a.ErrMessage = fmt.Sprintf("url ไม่ถูกต้อง: %q", u.URL)
		a.Duration = f.Now().Sub(start)
		return a
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		a.Outcome = model.OutcomeFatal
		a.ErrMessage = fmt.Sprintf("สร้าง request ไม่ได้: %v", err)
		a.Duration = f.Now().Sub(start)
		return a
	}

	for k, vs := range hdr {
		if hopByHop[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")

	// wrote บอกว่า body ถูกเขียนออก socket ครบหรือยัง เป็นตัวชี้ขาดว่า retry ได้ไหม
	var wrote atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				wrote.Store(true)
			}
		},
	}))

	resp, err := f.Client.Do(req)
	if err != nil {
		a.Duration = f.Now().Sub(start)
		a.ErrMessage = err.Error()
		a.Outcome = Classify(AttemptResult{Err: err, WroteRequest: wrote.Load()})
		return a
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, f.MaxResponseBytes+1)
	raw, readErr := io.ReadAll(limited)
	a.Duration = f.Now().Sub(start)
	a.HTTPStatus = resp.StatusCode

	if readErr != nil {
		a.ErrMessage = fmt.Sprintf("อ่าน response ไม่สำเร็จ: %v", readErr)
		a.Outcome = Classify(AttemptResult{Err: readErr, WroteRequest: true})
		return a
	}
	if int64(len(raw)) > f.MaxResponseBytes {
		// ไม่ตัดแล้วส่งต่อ เพราะ response ที่ถูกตัดคือ JSON พังที่ caller แปลไม่ออก
		a.ErrMessage = fmt.Sprintf("response ใหญ่เกิน %d ไบต์", f.MaxResponseBytes)
		a.Outcome = Classify(AttemptResult{Err: errors.New("response too large"), WroteRequest: true})
		return a
	}

	a.Body = raw
	a.Outcome = Classify(AttemptResult{Status: resp.StatusCode, WroteRequest: wrote.Load()})
	return a
}
