package store

import (
	"database/sql"
	"time"

	_ "github.com/lib/pq"
)

// Open เปิด connection pool พร้อมค่าที่เหมาะกับ worker หลายร้อยตัว
func Open(dsn string) (*sql.DB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(10 * time.Minute)
	return db, nil
}
