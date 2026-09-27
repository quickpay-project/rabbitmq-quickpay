package forward

import (
	"errors"
	"testing"

	"github.com/celalsahinaltinisik/internal/model"
)

func TestClassifyMatchesSpecTable(t *testing.T) {
	someErr := errors.New("boom")
	cases := []struct {
		name string
		in   AttemptResult
		want model.Outcome
	}{
		// สำเร็จ
		{"200", AttemptResult{Status: 200}, model.OutcomeSuccess},
		{"201", AttemptResult{Status: 201}, model.OutcomeSuccess},

		// ยังไม่ได้ส่ง body ออกไป = ปลอดภัยที่จะลอง url ถัดไป
		{"ต่อไม่ติด", AttemptResult{Err: someErr, WroteRequest: false}, model.OutcomeRetryable},

		// ส่ง body ไปแล้วแต่พัง = ไม่รู้ว่าออเดอร์เกิดหรือยัง ห้ามลองใหม่
		{"พังหลังส่ง body", AttemptResult{Err: someErr, WroteRequest: true}, model.OutcomeFatal},

		// ปลายทางปฏิเสธก่อนประมวลผล
		{"404", AttemptResult{Status: 404}, model.OutcomeRetryable},
		{"429", AttemptResult{Status: 429}, model.OutcomeRetryable},
		{"502", AttemptResult{Status: 502}, model.OutcomeRetryable},
		{"503", AttemptResult{Status: 503}, model.OutcomeRetryable},

		// คำขอผิดเอง ลองกี่ทีก็ผิด
		{"400", AttemptResult{Status: 400}, model.OutcomeFatal},
		{"401", AttemptResult{Status: 401}, model.OutcomeFatal},
		{"403", AttemptResult{Status: 403}, model.OutcomeFatal},
		{"422", AttemptResult{Status: 422}, model.OutcomeFatal},

		// แอปรับคำขอไปแล้ว อาจสร้างออเดอร์ไปบางส่วน
		{"500", AttemptResult{Status: 500}, model.OutcomeFatal},
		{"504", AttemptResult{Status: 504}, model.OutcomeFatal},

		// 5xx อื่นที่ไม่ได้อยู่ในรายการ — เลือกทางปลอดภัยไว้ก่อน
		{"501", AttemptResult{Status: 501}, model.OutcomeFatal},
		{"505", AttemptResult{Status: 505}, model.OutcomeFatal},

		// 3xx ไม่ตาม redirect เอง ถือว่าตั้ง url ผิด
		{"301", AttemptResult{Status: 301}, model.OutcomeFatal},
	}
	for _, c := range cases {
		if got := Classify(c.in); got != c.want {
			t.Errorf("%s: Classify = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestErrorWinsOverStatus(t *testing.T) {
	// ถ้ามี error แปลว่าไม่มี response ที่เชื่อถือได้ ต้องดูที่ WroteRequest อย่างเดียว
	got := Classify(AttemptResult{Status: 200, Err: errors.New("reset"), WroteRequest: true})
	if got != model.OutcomeFatal {
		t.Fatalf("Classify = %q, want fatal", got)
	}
}

func TestNoStatusNoErrorIsFatal(t *testing.T) {
	// สถานะที่ไม่ควรเกิด — อย่าเงียบ ให้ถือว่า fatal จะได้เห็นใน log
	if got := Classify(AttemptResult{}); got != model.OutcomeFatal {
		t.Fatalf("Classify = %q, want fatal", got)
	}
}
