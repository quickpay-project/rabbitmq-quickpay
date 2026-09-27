package model

// Outcome คือผลของการยิง upstream หนึ่งครั้ง
// กติกาเดียวที่ใช้ตัดสิน: retryable ได้เฉพาะเมื่อมั่นใจว่าคำขอไปไม่ถึงแอปปลายทาง
type Outcome string

const (
	OutcomeSuccess   Outcome = "success"
	OutcomeRetryable Outcome = "retryable"
	OutcomeFatal     Outcome = "fatal"
)

// RequestStatus คือค่าในคอลัมน์ request_logs.status
type RequestStatus string

const (
	StatusPending    RequestStatus = "pending"
	StatusSuccess    RequestStatus = "success"
	StatusFailed     RequestStatus = "failed"
	StatusNoUpstream RequestStatus = "no_upstream"
	StatusTimeout    RequestStatus = "timeout"
	StatusExpired    RequestStatus = "expired"
)
