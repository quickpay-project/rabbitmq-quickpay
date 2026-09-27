//go:build integration

package store

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// testDB สร้าง schema ชั่วคราวต่อ test หนึ่งตัว แล้ว drop ทิ้งตอนจบ
// ทำให้รันกับ dev DB จริงได้โดยไม่แตะตารางจริง
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("ตั้ง TEST_DATABASE_URL เพื่อรัน integration test")
	}

	admin, err := sql.Open("postgres", base)
	if err != nil {
		t.Fatalf("เปิด connection ไม่ได้: %v", err)
	}
	schema := fmt.Sprintf("mqtest_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatalf("สร้าง schema ไม่ได้: %v", err)
	}

	db, err := sql.Open("postgres", base+" search_path="+schema)
	if err != nil {
		t.Fatalf("เปิด connection ที่ schema ใหม่ไม่ได้: %v", err)
	}

	t.Cleanup(func() {
		_ = db.Close()
		if _, err := admin.Exec("DROP SCHEMA " + schema + " CASCADE"); err != nil {
			t.Logf("ลบ schema %s ไม่สำเร็จ: %v", schema, err)
		}
		_ = admin.Close()
	})
	return db
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	err := db.QueryRow(
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_name = $1 AND table_schema = current_schema()`, name).Scan(&n)
	if err != nil {
		t.Fatalf("เช็คตาราง %s ไม่ได้: %v", name, err)
	}
	return n > 0
}
