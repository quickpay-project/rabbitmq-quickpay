package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log"
	"sort"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// advisoryLockKey กันหลาย replica รัน migration ชนกันตอน start พร้อมกัน
const advisoryLockKey = 8412739

// Migrate รันไฟล์ใน migrations/ ตามลำดับชื่อ ข้ามอันที่เคยรันแล้ว
// ปลอดภัยที่จะเรียกทุกครั้งตอน start
func Migrate(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		return fmt.Errorf("ขอ advisory lock ไม่ได้: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, advisoryLockKey)
	}()

	if _, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("สร้าง schema_migrations ไม่ได้: %w", err)
	}

	// pgcrypto เป็น best-effort นอก transaction ของ migration โดยเจตนา
	// ถ้าอยู่ในไฟล์ migration แล้ว DB user ไม่มีสิทธิ์สร้าง extension migration จะล้มทั้งก้อน
	// และบน Postgres 13+ ก็ไม่ต้องใช้เลยเพราะ gen_random_uuid() เป็น built-in แล้ว
	if _, err := conn.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		log.Printf("⚠️  สร้าง extension pgcrypto ไม่ได้: %v (ข้ามไป — Postgres 13+ มี gen_random_uuid() ในตัว)", err)
	}

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var exists bool
		if err := conn.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}

		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("รัน migration %s ไม่สำเร็จ: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
