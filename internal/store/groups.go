package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/celalsahinaltinisik/internal/model"
)

type Store struct{ db *sql.DB }

func New(db *sql.DB) *Store { return &Store{db: db} }

// LoadGroups อ่าน desired state ทั้งระบบด้วย query เดียว
// LEFT JOIN ทำให้ group ที่ไม่มี url ที่ active ยังถูกคืนมาพร้อม URLs ว่าง
// reconciler จะเป็นคนตัดสินว่ามันควรเป็น degraded
func (s *Store) LoadGroups(ctx context.Context) ([]model.GroupSpec, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT g.id, g.group_name, g.worker_count, g.upstream_timeout_ms,
		       g.rpc_timeout_ms, g.ref_field, u.id, u.url
		FROM message_group g
		LEFT JOIN message_group_url u
		       ON u.message_group_id = g.id AND u.is_active
		ORDER BY g.group_name, u.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.GroupSpec
	byID := map[string]int{}

	for rows.Next() {
		var (
			id, name, refField   string
			workers, upMS, rpcMS int
			urlID                sql.NullInt64
			urlStr               sql.NullString
		)
		if err := rows.Scan(&id, &name, &workers, &upMS, &rpcMS, &refField, &urlID, &urlStr); err != nil {
			return nil, err
		}

		idx, seen := byID[id]
		if !seen {
			out = append(out, model.GroupSpec{
				ID:              id,
				Name:            name,
				WorkerCount:     workers,
				UpstreamTimeout: time.Duration(upMS) * time.Millisecond,
				RPCTimeout:      time.Duration(rpcMS) * time.Millisecond,
				RefField:        refField,
			})
			idx = len(out) - 1
			byID[id] = idx
		}
		if urlID.Valid && urlStr.Valid {
			out[idx].URLs = append(out[idx].URLs, model.URLSpec{ID: urlID.Int64, URL: urlStr.String})
		}
	}
	return out, rows.Err()
}
