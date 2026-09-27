//go:build integration

package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/celalsahinaltinisik/internal/model"
)

func newTrace(t *testing.T, db *sql.DB, s *Store, gid, name string, body []byte) string {
	t.Helper()
	var traceID string
	if err := db.QueryRow(`SELECT gen_random_uuid()`).Scan(&traceID); err != nil {
		t.Fatalf("สร้าง uuid ไม่ได้: %v", err)
	}
	err := s.BeginRequest(context.Background(), BeginRequestInput{
		TraceID: traceID, GroupID: gid, GroupName: name,
		ClientIP: "1.2.3.4", Body: body, BusinessRef: "ORDER-1",
	})
	if err != nil {
		t.Fatalf("BeginRequest: %v", err)
	}
	return traceID
}

func statusOf(t *testing.T, db *sql.DB, traceID string) (string, sql.NullInt64) {
	t.Helper()
	var st string
	var hs sql.NullInt64
	err := db.QueryRow(
		`SELECT status, http_status FROM request_logs WHERE trace_id = $1`, traceID).Scan(&st, &hs)
	if err != nil {
		t.Fatalf("อ่าน request_logs: %v", err)
	}
	return st, hs
}

func TestBeginRequestInsertsPending(t *testing.T) {
	db := migratedDB(t)
	s := New(db)
	gid := insertGroup(t, db, "withdraw")

	id := newTrace(t, db, s, gid, "withdraw", []byte(`{"amount":100}`))
	st, hs := statusOf(t, db, id)
	if st != string(model.StatusPending) {
		t.Errorf("status = %q, want pending", st)
	}
	if hs.Valid {
		t.Error("http_status ต้องยังว่างตอนเริ่ม")
	}
}

// Review Focus #3 — body ที่ไม่ใช่ JSON ต้องไม่ทำให้ INSERT ล้ม
func TestBeginRequestAcceptsNonJSONBody(t *testing.T) {
	db := migratedDB(t)
	s := New(db)
	gid := insertGroup(t, db, "withdraw")

	for _, body := range [][]byte{[]byte(""), []byte("ไม่ใช่ json"), {0xff, 0x00}, nil} {
		var traceID string
		_ = db.QueryRow(`SELECT gen_random_uuid()`).Scan(&traceID)
		err := s.BeginRequest(context.Background(), BeginRequestInput{
			TraceID: traceID, GroupID: gid, GroupName: "withdraw", Body: body,
		})
		if err != nil {
			t.Fatalf("body %q ต้อง insert ได้ แต่ได้ error: %v", body, err)
		}
	}
}

func TestRecordAttemptsWritesEveryTry(t *testing.T) {
	db := migratedDB(t)
	s := New(db)
	gid := insertGroup(t, db, "withdraw")
	uid := insertURL(t, db, gid, "https://a.example.com", true)
	id := newTrace(t, db, s, gid, "withdraw", []byte(`{}`))

	err := s.RecordAttempts(context.Background(), id, []AttemptInput{
		{Seq: 1, URLID: uid, URL: "https://a.example.com", HTTPStatus: 502,
			DurationMS: 120, Outcome: model.OutcomeRetryable, ErrMessage: "bad gateway"},
		{Seq: 2, URLID: uid, URL: "https://b.example.com", HTTPStatus: 200,
			DurationMS: 340, Outcome: model.OutcomeSuccess, ResponseBody: `{"code":0}`},
	})
	if err != nil {
		t.Fatalf("RecordAttempts: %v", err)
	}

	var n int
	_ = db.QueryRow(`SELECT count(*) FROM attempt_logs WHERE trace_id = $1`, id).Scan(&n)
	if n != 2 {
		t.Fatalf("attempt_logs = %d แถว, want 2", n)
	}

	var url string
	var outcome string
	err = db.QueryRow(
		`SELECT url, outcome FROM attempt_logs WHERE trace_id = $1 AND seq = 1`, id).Scan(&url, &outcome)
	if err != nil {
		t.Fatalf("อ่าน attempt seq 1: %v", err)
	}
	if outcome != string(model.OutcomeRetryable) {
		t.Errorf("outcome = %q, want retryable", outcome)
	}
}

func TestRecordAttemptsWithEmptySliceIsNoop(t *testing.T) {
	db := migratedDB(t)
	s := New(db)
	gid := insertGroup(t, db, "withdraw")
	id := newTrace(t, db, s, gid, "withdraw", []byte(`{}`))

	if err := s.RecordAttempts(context.Background(), id, nil); err != nil {
		t.Fatalf("slice ว่างต้องไม่ error: %v", err)
	}
}

func TestFinishRequestSetsWorkerOutcome(t *testing.T) {
	db := migratedDB(t)
	s := New(db)
	gid := insertGroup(t, db, "withdraw")
	id := newTrace(t, db, s, gid, "withdraw", []byte(`{}`))

	err := s.FinishRequest(context.Background(), FinishRequestInput{
		TraceID: id, Status: model.StatusSuccess,
		ResponseBody: []byte(`{"code":0}`), AttemptCount: 2, TotalMS: 460,
	})
	if err != nil {
		t.Fatalf("FinishRequest: %v", err)
	}
	st, _ := statusOf(t, db, id)
	if st != string(model.StatusSuccess) {
		t.Errorf("status = %q, want success", st)
	}
}

func TestMarkClientOutcomeOnlyOverwritesPendingStatus(t *testing.T) {
	db := migratedDB(t)
	s := New(db)
	ctx := context.Background()
	gid := insertGroup(t, db, "withdraw")

	// ลำดับ ก: worker จบก่อน แล้ว caller ค่อย timeout
	a := newTrace(t, db, s, gid, "withdraw", []byte(`{}`))
	_ = s.FinishRequest(ctx, FinishRequestInput{TraceID: a, Status: model.StatusSuccess, AttemptCount: 1})
	if err := s.MarkClientOutcome(ctx, a, 504); err != nil {
		t.Fatalf("MarkClientOutcome: %v", err)
	}
	st, hs := statusOf(t, db, a)
	if st != string(model.StatusSuccess) {
		t.Errorf("ลำดับ ก: status = %q, want success — worker คือความจริง", st)
	}
	if hs.Int64 != 504 {
		t.Errorf("ลำดับ ก: http_status = %d, want 504 — ต้องบันทึกสิ่งที่ caller ได้รับ", hs.Int64)
	}

	// ลำดับ ข: caller timeout ก่อน แล้ว worker ค่อยจบ — ต้องได้ผลเดียวกัน
	b := newTrace(t, db, s, gid, "withdraw", []byte(`{}`))
	_ = s.MarkClientOutcome(ctx, b, 504)
	stMid, _ := statusOf(t, db, b)
	if stMid != string(model.StatusTimeout) {
		t.Errorf("ลำดับ ข: ระหว่างทาง status = %q, want timeout", stMid)
	}
	_ = s.FinishRequest(ctx, FinishRequestInput{TraceID: b, Status: model.StatusSuccess, AttemptCount: 1})
	st2, hs2 := statusOf(t, db, b)
	if st2 != string(model.StatusSuccess) {
		t.Errorf("ลำดับ ข: status = %q, want success", st2)
	}
	if hs2.Int64 != 504 {
		t.Errorf("ลำดับ ข: http_status = %d, want 504", hs2.Int64)
	}
}

func TestDangerousCaseQueryFromSpec(t *testing.T) {
	db := migratedDB(t)
	s := New(db)
	ctx := context.Background()
	gid := insertGroup(t, db, "withdraw")

	danger := newTrace(t, db, s, gid, "withdraw", []byte(`{}`))
	_ = s.FinishRequest(ctx, FinishRequestInput{TraceID: danger, Status: model.StatusSuccess, AttemptCount: 1})
	_ = s.MarkClientOutcome(ctx, danger, 504)

	normal := newTrace(t, db, s, gid, "withdraw", []byte(`{}`))
	_ = s.FinishRequest(ctx, FinishRequestInput{TraceID: normal, Status: model.StatusSuccess, AttemptCount: 1})
	_ = s.MarkClientOutcome(ctx, normal, 200)

	var n int
	err := db.QueryRow(
		`SELECT count(*) FROM request_logs WHERE http_status = 504 AND status = 'success'`).Scan(&n)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 1 {
		t.Fatalf("เจอ %d แถว, want 1 — query หาเคส caller คิดว่าล้มแต่ออเดอร์เกิดจริงต้องใช้งานได้", n)
	}
}
