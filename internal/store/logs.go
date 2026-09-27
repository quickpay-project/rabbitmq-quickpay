package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/celalsahinaltinisik/internal/model"
)

type BeginRequestInput struct {
	TraceID       string
	GroupID       string
	GroupName     string
	CallerTraceID string
	ClientIP      string
	Body          []byte
	BusinessRef   string
}

// BeginRequest เขียนแถวตั้งแต่รับ request เข้ามา ก่อนจะรู้ผลอะไรเลย
// ทำให้ request ที่ตายกลางทางยังเหลือร่องรอย ต่างจากระบบเก่าที่ insert ตอนจบอย่างเดียว
func (s *Store) BeginRequest(ctx context.Context, in BeginRequestInput) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO request_logs
		  (trace_id, message_group_id, group_name, status, business_ref,
		   caller_trace_id, client_ip, request_body)
		VALUES ($1, $2, $3, $4, NULLIF($5,''), NULLIF($6,''), NULLIF($7,''), $8::jsonb)`,
		in.TraceID, nullUUID(in.GroupID), in.GroupName, string(model.StatusPending),
		in.BusinessRef, in.CallerTraceID, in.ClientIP, string(model.JSONOrRaw(in.Body)))
	return err
}

type AttemptInput struct {
	Seq          int
	URLID        int64
	URL          string
	HTTPStatus   int
	DurationMS   int
	Outcome      model.Outcome
	ResponseBody string
	ErrMessage   string
}

// RecordAttempts เขียนทุก attempt ของ request หนึ่งในคำสั่งเดียว
func (s *Store) RecordAttempts(ctx context.Context, traceID string, attempts []AttemptInput) error {
	if len(attempts) == 0 {
		return nil
	}
	var (
		placeholders []string
		args         []any
	)
	// response_body อยู่ในคอลัมน์ชุดเดียวกัน ไม่แยกไป UPDATE ตามหลัง
	// เพราะ attempt ที่สำเร็จมี response body แทบทุกครั้ง แบบแยกจึงกลายเป็น 1 INSERT + N UPDATE
	// นอกจากช้ากว่าแล้ว ถ้า UPDATE ตัวท้าย ๆ ล้ม แถวก่อนหน้าจะถูก commit ไปแล้วโดยไม่มี response_body
	// แต่ทั้ง call คืน error ทำให้แยกไม่ออกว่า attempt ไม่ถูกบันทึกเลยหรือบันทึกแล้วแต่ response หาย
	// คำสั่งเดียวทำให้ผลมีแค่สองแบบ: ทุกแถวครบ หรือไม่มีแถวไหนเลย
	for i, a := range attempts {
		n := i * 9
		placeholders = append(placeholders, fmt.Sprintf(
			"($%d,$%d,NULLIF($%d,0),$%d,NULLIF($%d,0),$%d,$%d,NULLIF($%d,''),NULLIF($%d,''))",
			n+1, n+2, n+3, n+4, n+5, n+6, n+7, n+8, n+9))
		args = append(args, traceID, a.Seq, a.URLID, a.URL,
			a.HTTPStatus, a.DurationMS, string(a.Outcome), a.ResponseBody, a.ErrMessage)
	}
	query := `INSERT INTO attempt_logs
	  (trace_id, seq, message_group_url_id, url, http_status, duration_ms, outcome,
	   response_body, error_message)
	  VALUES ` + strings.Join(placeholders, ",")
	_, err := s.db.ExecContext(ctx, query, args...)
	return err
}

type FinishRequestInput struct {
	TraceID      string
	Status       model.RequestStatus
	ResponseBody []byte
	AttemptCount int
	TotalMS      int
	ErrMessage   string
}

// FinishRequest เขียนผลจริงจากฝั่ง worker — worker คือความจริง จึงทับได้เสมอ
func (s *Store) FinishRequest(ctx context.Context, in FinishRequestInput) error {
	var body any
	if len(in.ResponseBody) > 0 {
		body = string(model.JSONOrRaw(in.ResponseBody))
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE request_logs
		SET status = $2,
		    response_body = COALESCE($3::jsonb, response_body),
		    attempt_count = $4,
		    total_ms = $5,
		    error_message = NULLIF($6,''),
		    finished_at = now()
		WHERE trace_id = $1`,
		in.TraceID, string(in.Status), body, in.AttemptCount, in.TotalMS, in.ErrMessage)
	return err
}

// MarkClientOutcome บันทึกสิ่งที่ caller ได้รับจริง
// ตั้ง status เป็น timeout ได้เฉพาะตอนที่ worker ยังไม่เขียนผลของตัวเองลงมา
func (s *Store) MarkClientOutcome(ctx context.Context, traceID string, httpStatus int) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE request_logs
		SET http_status = $2,
		    status = CASE WHEN status = $3 THEN $4 ELSE status END,
		    finished_at = COALESCE(finished_at, now())
		WHERE trace_id = $1`,
		traceID, httpStatus, string(model.StatusPending), string(model.StatusTimeout))
	return err
}

func nullUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
}
