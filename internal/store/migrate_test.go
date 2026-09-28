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
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate ครั้งที่ 2 ต้องไม่ error: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("นับ schema_migrations ไม่ได้: %v", err)
	}
	if n != 1 {
		t.Errorf("schema_migrations มี %d แถว, want 1 — migration ถูกรันซ้ำ", n)
	}
}
