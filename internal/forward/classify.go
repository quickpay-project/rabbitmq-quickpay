package forward

import "github.com/celalsahinaltinisik/internal/model"

// AttemptResult คือผลดิบของการยิง upstream หนึ่งครั้ง ก่อนถูกจำแนก
type AttemptResult struct {
	Status int
	// WroteRequest มาจาก httptrace บอกว่า request ถูกเขียนออก socket ครบแล้วหรือยัง
	// เป็นตัวชี้ขาดว่าปลายทางมีโอกาสเห็นคำขอนี้หรือไม่
	WroteRequest bool
	Err          error
}

// Classify ตัดสินว่าจะลอง url ถัดไปได้หรือไม่
//
// กติกาเดียว: retryable ได้เฉพาะเมื่อมั่นใจว่าคำขอไปไม่ถึงแอปปลายทาง
// เพราะนี่เป็นงานการเงิน การลองซ้ำผิดจังหวะ = ถอนเงินหรือสร้าง QR ซ้ำให้ลูกค้าจริง
// ห้ามทำให้ตารางนี้ config ได้ — เป็นความถูกต้องทางธุรกิจ ไม่ใช่การปรับจูน
func Classify(r AttemptResult) model.Outcome {
	if r.Err != nil {
		if r.WroteRequest {
			// ส่ง body ออกไปแล้วถึงพัง (timeout รอ response, connection reset)
			// แอปปลายทางอาจประมวลผลไปแล้ว ลองใหม่ = เสี่ยงซ้ำ
			return model.OutcomeFatal
		}
		// ยังเขียน request ไม่เสร็จ (DNS fail, connection refused, TLS handshake fail)
		// แอปปลายทางไม่มีทางเห็นคำขอนี้ ลอง url ถัดไปได้
		return model.OutcomeRetryable
	}

	switch {
	case r.Status >= 200 && r.Status < 300:
		return model.OutcomeSuccess
	case r.Status == 404, r.Status == 429, r.Status == 502, r.Status == 503:
		// ถูกปฏิเสธหรือต่อ backend ไม่ได้ตั้งแต่ชั้น proxy — แอปยังไม่ได้ประมวลผล
		return model.OutcomeRetryable
	default:
		// รวม 400/401/403/422 (คำขอผิดเอง)
		// และ 500/504 กับ 5xx อื่น (แอปรับไปแล้ว อาจสร้างออเดอร์ไปบางส่วน)
		// และ 3xx (ตั้ง url ผิด ไม่ตาม redirect เอง)
		return model.OutcomeFatal
	}
}
