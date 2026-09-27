//go:build integration

package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/celalsahinaltinisik/internal/model"
)

func migratedDB(t *testing.T) *sql.DB {
	t.Helper()
	db := testDB(t)
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func insertGroup(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var id string
	err := db.QueryRow(
		`INSERT INTO message_group (group_name) VALUES ($1) RETURNING id`, name).Scan(&id)
	if err != nil {
		t.Fatalf("insert group %s: %v", name, err)
	}
	return id
}

func insertURL(t *testing.T, db *sql.DB, groupID, url string, active bool) int64 {
	t.Helper()
	var id int64
	err := db.QueryRow(
		`INSERT INTO message_group_url (message_group_id, url, is_active)
		 VALUES ($1, $2, $3) RETURNING id`, groupID, url, active).Scan(&id)
	if err != nil {
		t.Fatalf("insert url %s: %v", url, err)
	}
	return id
}

func TestLoadGroupsReturnsOnlyActiveURLs(t *testing.T) {
	db := migratedDB(t)
	gid := insertGroup(t, db, "withdraw")
	insertURL(t, db, gid, "https://a.example.com", true)
	insertURL(t, db, gid, "https://b.example.com", false)
	insertURL(t, db, gid, "https://c.example.com", true)

	groups, err := New(db).LoadGroups(context.Background())
	if err != nil {
		t.Fatalf("LoadGroups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
	g := groups[0]
	if len(g.URLs) != 2 {
		t.Fatalf("URLs = %d, want 2 (ตัวที่ is_active=false ต้องไม่มา)", len(g.URLs))
	}
	if g.URLs[0].URL != "https://a.example.com" || g.URLs[1].URL != "https://c.example.com" {
		t.Errorf("URLs = %+v", g.URLs)
	}
	if g.URLs[0].ID == 0 {
		t.Error("URLSpec.ID ต้องถูกเติม เพราะ attempt_logs ต้องอ้างกลับมาได้")
	}
}

func TestLoadGroupsIncludesGroupWithNoActiveURL(t *testing.T) {
	db := migratedDB(t)
	gid := insertGroup(t, db, "promptpay")
	insertURL(t, db, gid, "https://dead.example.com", false)

	groups, err := New(db).LoadGroups(context.Background())
	if err != nil {
		t.Fatalf("LoadGroups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1 — group ที่ไม่มี url ต้องยังถูกคืนมาเพื่อให้ mark degraded ได้", len(groups))
	}
	if groups[0].HasUpstream() {
		t.Error("HasUpstream ต้องเป็น false")
	}
}

func TestLoadGroupsAppliesColumnDefaults(t *testing.T) {
	db := migratedDB(t)
	insertGroup(t, db, "deposit")

	groups, _ := New(db).LoadGroups(context.Background())
	g := groups[0]
	if g.WorkerCount != 50 {
		t.Errorf("WorkerCount = %d, want 50", g.WorkerCount)
	}
	if g.UpstreamTimeout != 30*time.Second {
		t.Errorf("UpstreamTimeout = %v, want 30s", g.UpstreamTimeout)
	}
	if g.RPCTimeout != 60*time.Second {
		t.Errorf("RPCTimeout = %v, want 60s", g.RPCTimeout)
	}
	if g.RefField != "customer_order_id" {
		t.Errorf("RefField = %q, want customer_order_id", g.RefField)
	}
	if g.ID == "" {
		t.Error("ID (uuid) ต้องถูกเติม")
	}
}

func TestLoadGroupsSortedByName(t *testing.T) {
	db := migratedDB(t)
	insertGroup(t, db, "withdraw")
	insertGroup(t, db, "deposit")

	groups, _ := New(db).LoadGroups(context.Background())
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(groups))
	}
	if groups[0].Name != "deposit" || groups[1].Name != "withdraw" {
		t.Errorf("ลำดับ = %s,%s want deposit,withdraw", groups[0].Name, groups[1].Name)
	}
}

func TestLoadGroupsEmptyDB(t *testing.T) {
	db := migratedDB(t)
	groups, err := New(db).LoadGroups(context.Background())
	if err != nil {
		t.Fatalf("LoadGroups: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("groups = %d, want 0", len(groups))
	}
	_ = model.GroupSpec{}
}
