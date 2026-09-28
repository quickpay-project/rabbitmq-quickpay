//go:build integration

package store

import (
	"context"
	"testing"
)

func TestMigrateCreatesAllTables(t *testing.T) {
	db := testDB(t)
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for _, name := range []string{"message_group", "message_group_url", "request_logs", "attempt_logs", "blocked_ip", "schema_migrations"} {
		if !tableExists(t, db, name) {
			t.Errorf("ไม่พบตาราง %s", name)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate ครั้งที่ 1: %v", err)
	}
	// เทียบก่อน/หลัง ไม่ผูกกับจำนวน migration ที่มี ณ วันที่เขียนเทสต์
	// ไม่งั้นการเพิ่มไฟล์ migration ใหม่จะทำให้เทสต์นี้แดงทั้งที่ไม่มีอะไรเสีย
	var before int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&before); err != nil {
		t.Fatalf("นับ schema_migrations ไม่ได้: %v", err)
	}
	if before == 0 {
		t.Fatal("Migrate ครั้งแรกไม่ได้บันทึกอะไรเลย")
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate ครั้งที่ 2 ต้องไม่ error: %v", err)
	}
	var after int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&after); err != nil {
		t.Fatalf("นับ schema_migrations ไม่ได้: %v", err)
	}
	if after != before {
		t.Errorf("schema_migrations เปลี่ยนจาก %d เป็น %d — migration ถูกรันซ้ำ", before, after)
	}
}
