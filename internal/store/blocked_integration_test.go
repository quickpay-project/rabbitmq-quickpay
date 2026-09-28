//go:build integration

package store

import (
	"context"
	"testing"
)

// RecordBlocked ต้องนับเพิ่มในแถวเดิม ไม่ใช่สร้างแถวใหม่ทุกครั้ง
// ถ้าสร้างใหม่ ตารางจะโตตามปริมาณ traffic แล้วกลายเป็นช่องขยายผล DoS
func TestRecordBlockedCountsInPlace(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	s := New(db)

	in := BlockedInput{ClientIP: "2400:6180:0:d0::17d0:c001", Path: "/deposit",
		Reason: BlockedNotInAllowlist}
	for i := 0; i < 5; i++ {
		if err := s.RecordBlocked(ctx, in); err != nil {
			t.Fatalf("ครั้งที่ %d: %v", i+1, err)
		}
	}

	var rows, count int64
	if err := db.QueryRow(`SELECT count(*), coalesce(max(count),0) FROM blocked_ip`).
		Scan(&rows, &count); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("อยากได้ 1 แถว แต่ได้ %d", rows)
	}
	if count != 5 {
		t.Fatalf("อยากได้ count=5 แต่ได้ %d", count)
	}

	var first, last string
	if err := db.QueryRow(`SELECT first_seen::text, last_seen::text FROM blocked_ip`).
		Scan(&first, &last); err != nil {
		t.Fatal(err)
	}
	if first == last {
		t.Log("first_seen เท่ากับ last_seen (เขียนเร็วมากในทรานแซกชันเดียวกัน) ยอมรับได้")
	}
}

// เหตุผลต่างกันต้องแยกแถว เพราะแก้คนละทาง
func TestRecordBlockedSeparatesReasons(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	s := New(db)
	for _, r := range []string{BlockedNotInAllowlist, BlockedMissingIPHeader} {
		if err := s.RecordBlocked(ctx, BlockedInput{ClientIP: "1.2.3.4", Path: "/deposit", Reason: r}); err != nil {
			t.Fatal(err)
		}
	}
	var n int64
	if err := db.QueryRow(`SELECT count(*) FROM blocked_ip`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("อยากได้ 2 แถว แต่ได้ %d", n)
	}
}

// client_ip ว่างคือเคส missing_trusted_header ต้องบันทึกได้ ไม่ใช่ล้มเงียบ ๆ
// เคยพลาดมาแล้วเพราะใส่ NULLIF ให้คอลัมน์ที่เป็น NOT NULL
func TestRecordBlockedAcceptsEmptyIP(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	s := New(db)
	for i := 0; i < 2; i++ {
		if err := s.RecordBlocked(ctx, BlockedInput{ClientIP: "", Path: "/deposit",
			Reason: BlockedMissingIPHeader}); err != nil {
			t.Fatalf("ครั้งที่ %d: %v", i+1, err)
		}
	}
	var n int64
	if err := db.QueryRow(
		`SELECT count FROM blocked_ip WHERE reason=$1`, BlockedMissingIPHeader).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("อยากได้ count=2 แต่ได้ %d", n)
	}
}
