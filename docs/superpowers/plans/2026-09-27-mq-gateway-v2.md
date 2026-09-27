# MQ Gateway v2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** สร้าง HTTP→AMQP→upstream gateway ตัวใหม่ที่ flow ทุกตัวเกิดจากแถวใน Postgres แทนการ hardcode ใน Go และ env

**Architecture:** binary เดียว (`cmd/gateway`) ที่มี reconciler loop คอยเทียบ desired state จาก DB กับ flow ที่รันอยู่จริง แล้วสร้าง/ปรับ/ปิด flow ให้ตรงกัน แต่ละ flow = 1 queue + 1 consumer + worker pool + 1 HTTP endpoint ที่ชื่อเดียวกับ `group_name` ตัว engine ไม่รู้จัก payload เลย ส่งต่อแบบ passthrough ทั้งหมด ทุก request มี `trace_id` ที่ไหลจาก HTTP ผ่าน AMQP ไปถึง log ทุกแถว

**Tech Stack:** Go 1.23+, `github.com/rabbitmq/amqp091-go`, `github.com/lib/pq`, `github.com/google/uuid`, Postgres 13+, RabbitMQ 3.12

**Spec:** `docs/superpowers/specs/2026-09-25-mq-gateway-refactor-design.md`

---

## Global Constraints

- **module path คือ `github.com/celalsahinaltinisik`** — ห้ามเปลี่ยน เพราะโค้ดเก่าที่ยังรัน production import ด้วยชื่อนี้ แพ็กเกจใหม่ทั้งหมดจึงเป็น `github.com/celalsahinaltinisik/internal/...`
- **ห้ามแตะไฟล์ของระบบเก่า**: `main.go`, `rabbitMQ/`, `controllers/`, `route/`, `exceptions/` — ยกเว้นข้อยกเว้นที่ระบุไว้ใน Task 1 (ลบ `udpsocket/`)
- `go.mod` ตั้ง `go 1.23` และ **ไม่มีบรรทัด `toolchain`** (เครื่อง dev ใช้ go1.25.14, image ใช้ golang:1.23-alpine ต้อง build ได้ทั้งคู่)
- **`QUEUE_PREFIX` ห้ามว่าง** — service ต้อง refuse to start ถ้าไม่ได้ตั้ง (spec §8.3) ชื่อ queue จริง = `QUEUE_PREFIX + group_name`
- **`ALLOWED_IPS` ว่าง = ปฏิเสธทุกคน** จะเปิดหมดต้องเขียน `*` ชัดเจน (spec §7.8)
- **ตารางจำแนก error ตาม spec §7.5 ห้ามแก้ให้ config ได้** — 504 / 500 / timeout หลังส่ง body = `fatal` ห้าม retry เด็ดขาด
- **consumer loop ต้อง return เสมอเมื่อ channel ปิด** ห้ามมี `select {}` หรือ block ถาวร (spec §6.5)
- ทุก task จบด้วย commit เดียวที่ test ผ่าน
- unit test ต้องรันได้โดยไม่ต้องมี Postgres/RabbitMQ — integration test อยู่หลัง build tag `integration` และ skip เองถ้าไม่มี env

## Review Focus

จุดที่ spec บอกเป็นนัยแต่ไม่มี task ไหนทดสอบตรง ๆ ถ้าไม่จงใจใส่ เรียงตามโอกาสที่จะกัดคนใช้จริง

1. **reply ที่มาถึงหลัง caller หมดเวลาไปแล้ว** → entry ใน correlation map ต้องถูกลบทิ้งเสมอ ไม่งั้น memory leak สะสมทุก request ที่ timeout (ทดสอบใน Task 8)
2. **`x-deadline` หาย/parse ไม่ได้** (ข้อความค้างจากเวอร์ชันก่อน หรือมีคน publish เอง) → worker ต้องไม่ panic และต้องถือว่าหมดอายุ ไม่ใช่ยิง upstream แบบไม่มีขอบเขตเวลา (ทดสอบใน Task 9)
3. **body ที่ไม่ใช่ JSON** (ว่าง, binary, JSON พัง) → `request_body JSONB` จะ insert ไม่ผ่านและทำให้ทั้ง request ล้ม ต้อง wrap เป็น `{"raw": "..."}` (ทดสอบใน Task 7)
4. **url ใน DB ที่ผิดรูป** (ไม่มี scheme, มี whitespace, ว่าง) → `http.NewRequest` error ทุกครั้ง ต้องนับเป็น attempt ที่ `fatal` พร้อม log ไม่ใช่ปล่อย panic หรือวน retry (ทดสอบใน Task 4)
5. **`group_name` จาก DB ที่ผิดกติกา** (ตัวใหญ่, เว้นวรรค, ยาวเกิน 64, ชนคำสงวน) → reconciler ต้องข้ามพร้อม log ไม่ใช่ทำให้ทั้ง loop ตายจน flow อื่นไม่ได้ขึ้น (ทดสอบใน Task 10)

---

## File Structure

โค้ดใหม่ทั้งหมดอยู่ใต้ `cmd/` กับ `internal/` ไม่ทับกับของเก่าเลย

| ไฟล์ | หน้าที่ |
|---|---|
| `internal/model/group.go` | `GroupSpec`, `URLSpec`, `Revision()`, `QueueName()`, `ValidateName()` — ชนิดข้อมูลล้วน ไม่ import อะไรในโปรเจกต์ |
| `internal/model/outcome.go` | `Outcome` (success/retryable/fatal), `RequestStatus` |
| `internal/config/config.go` | อ่าน env + validate + default |
| `internal/forward/classify.go` | ตารางจำแนก error ตาม spec §7.5 |
| `internal/forward/forwarder.go` | ยิง upstream ตามลำดับ url พร้อม budget เวลา |
| `internal/store/migrate.go` | migration ฝังใน binary + advisory lock |
| `internal/store/migrations/*.sql` | ไฟล์ schema |
| `internal/store/groups.go` | `LoadGroups` |
| `internal/store/logs.go` | `BeginRequest` / `RecordAttempt` / `FinishRequest` / `MarkTimeout` |
| `internal/amqpx/conn.go` | connection manager + auto-reconnect |
| `internal/amqpx/rpc.go` | RPC client pool + correlation map |
| `internal/flow/flow.go` | consumer + worker pool + drain |
| `internal/flow/registry.go` | ทะเบียน flow ที่รันอยู่ + สถานะ |
| `internal/reconcile/diff.go` | เทียบ desired vs actual → action (ฟังก์ชันบริสุทธิ์) |
| `internal/reconcile/loop.go` | วน reconcile + apply action |
| `internal/httpapi/router.go` | dynamic route + publish path |
| `internal/httpapi/clientip.go` | ดึง client IP + whitelist |
| `internal/httpapi/health.go` | `/healthz` `/readyz` |
| `cmd/gateway/main.go` | wiring + startup sequence + graceful shutdown |
| `Dockerfile.gateway` | multi-stage build ของ service ใหม่ (ของเก่ายังใช้ `Dockerfile` เดิม) |

**หมายเหตุที่ต่างจาก spec §5.1:** เพิ่ม `internal/model` ที่ spec ไม่ได้ระบุไว้ เพื่อให้ `store`, `flow`, `reconcile`, `forward` ใช้ชนิดข้อมูลร่วมกันได้โดยไม่เกิด import cycle — `model` ไม่ import แพ็กเกจอื่นในโปรเจกต์เลย

**หมายเหตุเรื่อง Dockerfile:** spec §8.1 เขียนว่าแทนที่ `Dockerfile` เดิม แต่ระหว่างที่ของเก่ายังรัน production เราต้องเก็บไฟล์เดิมไว้ จึงสร้างเป็น `Dockerfile.gateway` แยก แล้วค่อยเปลี่ยนชื่อตอน cutover เสร็จ (spec §8.5 ระยะที่ 4)

---

## Task 1: Foundation — module cleanup และชนิดข้อมูลร่วม

**Files:**
- Delete: `udpsocket/udplistener.go`, `udpsocket/udpsender.go`
- Modify: `go.mod`
- Create: `internal/model/group.go`, `internal/model/outcome.go`, `internal/model/group_test.go`

**Interfaces:**
- Consumes: ไม่มี (task แรก)
- Produces: `model.GroupSpec`, `model.URLSpec`, `model.Outcome`, `model.RequestStatus`,
  `GroupSpec.Revision() string`, `GroupSpec.QueueName(prefix string) string`,
  `GroupSpec.ValidateName() error`

- [ ] **Step 1: ลบโค้ดตายและแก้ go.mod**

`udpsocket/` เป็นโค้ด webcam ภาษาตุรกีจาก template ต้นทาง มี `func main()` ซ้อนกันสองตัวใน package เดียว ทำให้ `go build ./...` พังอยู่ทุกวันนี้ และไม่ได้อยู่ใน build target ของระบบเก่า (`go build .`)

```bash
git rm -r udpsocket/
```

แก้ `go.mod` ให้เป็นแบบนี้ทั้งไฟล์ (ลบ `gocv`, ลบ `toolchain`, ตั้ง `go 1.23`):

```
module github.com/celalsahinaltinisik

go 1.23

require (
	github.com/google/uuid v1.6.0
	github.com/lib/pq v1.10.9
	github.com/rabbitmq/amqp091-go v1.8.1
)
```

แล้วรัน:

```bash
go mod tidy
```

- [ ] **Step 2: ยืนยันว่าทั้งรีโป build ผ่านแล้ว**

Run: `go build ./... && go vet ./...`
Expected: ผ่านทั้งคู่ ไม่มี error เรื่อง `pkg-config` หรือ `gocv` อีก (ก่อนหน้านี้พังที่ `gocv.io/x/gocv: exec: "pkg-config": executable file not found`)

- [ ] **Step 3: เขียน test ที่ยังไม่ผ่านสำหรับ model**

สร้าง `internal/model/group_test.go`:

```go
package model

import "testing"

func TestQueueNameUsesPrefix(t *testing.T) {
	g := GroupSpec{Name: "withdraw"}
	if got := g.QueueName("v2."); got != "v2.withdraw" {
		t.Fatalf("QueueName = %q, want %q", got, "v2.withdraw")
	}
}

func TestRevisionStableAcrossURLOrder(t *testing.T) {
	a := GroupSpec{ID: "g1", Name: "withdraw", WorkerCount: 50,
		URLs: []URLSpec{{ID: 1, URL: "https://a"}, {ID: 2, URL: "https://b"}}}
	b := GroupSpec{ID: "g1", Name: "withdraw", WorkerCount: 50,
		URLs: []URLSpec{{ID: 2, URL: "https://b"}, {ID: 1, URL: "https://a"}}}
	if a.Revision() != b.Revision() {
		t.Fatal("Revision ต้องไม่ขึ้นกับลำดับของ URLs")
	}
}

func TestRevisionChangesWhenURLDeactivated(t *testing.T) {
	a := GroupSpec{ID: "g1", Name: "withdraw",
		URLs: []URLSpec{{ID: 1, URL: "https://a"}, {ID: 2, URL: "https://b"}}}
	b := GroupSpec{ID: "g1", Name: "withdraw",
		URLs: []URLSpec{{ID: 1, URL: "https://a"}}}
	if a.Revision() == b.Revision() {
		t.Fatal("Revision ต้องเปลี่ยนเมื่อ url หายไปหนึ่งตัว")
	}
}

func TestRevisionChangesWhenWorkerCountChanges(t *testing.T) {
	a := GroupSpec{ID: "g1", Name: "withdraw", WorkerCount: 50}
	b := GroupSpec{ID: "g1", Name: "withdraw", WorkerCount: 80}
	if a.Revision() == b.Revision() {
		t.Fatal("Revision ต้องเปลี่ยนเมื่อ worker_count เปลี่ยน")
	}
}

func TestValidateName(t *testing.T) {
	cases := []struct {
		name    string
		wantErr bool
	}{
		{"withdraw", false},
		{"withdraw-auto", false},
		{"deposit_v2", false},
		{"a", false},
		{"", true},
		{"Withdraw", true},        // ตัวใหญ่ไม่ได้ เพราะชื่อ queue และ path ต้องคาดเดาได้
		{"with draw", true},       // เว้นวรรคไม่ได้
		{"-withdraw", true},       // ห้ามขึ้นต้นด้วย -
		{"healthz", true},         // คำสงวน
		{"readyz", true},          // คำสงวน
		{"withdraw/../admin", true},
	}
	for _, c := range cases {
		err := GroupSpec{Name: c.name}.ValidateName()
		if (err != nil) != c.wantErr {
			t.Errorf("ValidateName(%q) err=%v, wantErr=%v", c.name, err, c.wantErr)
		}
	}
}

func TestValidateNameTooLong(t *testing.T) {
	long := ""
	for i := 0; i < 65; i++ {
		long += "a"
	}
	if err := (GroupSpec{Name: long}).ValidateName(); err == nil {
		t.Fatal("ชื่อยาว 65 ตัวอักษรต้องไม่ผ่าน")
	}
}

func TestActiveURLsEmptyMeansDegraded(t *testing.T) {
	g := GroupSpec{Name: "withdraw"}
	if g.HasUpstream() {
		t.Fatal("group ที่ไม่มี url ต้องรายงานว่าไม่มี upstream")
	}
}
```

- [ ] **Step 4: รัน test ให้เห็นว่า fail**

Run: `go test ./internal/model/ -v`
Expected: FAIL — คอมไพล์ไม่ผ่านเพราะยังไม่มี `GroupSpec`

- [ ] **Step 5: เขียน implementation ขั้นต่ำ**

สร้าง `internal/model/outcome.go`:

```go
package model

// Outcome คือผลของการยิง upstream หนึ่งครั้ง
// กติกาเดียวที่ใช้ตัดสิน: retryable ได้เฉพาะเมื่อมั่นใจว่าคำขอไปไม่ถึงแอปปลายทาง
type Outcome string

const (
	OutcomeSuccess   Outcome = "success"
	OutcomeRetryable Outcome = "retryable"
	OutcomeFatal     Outcome = "fatal"
)

// RequestStatus คือค่าในคอลัมน์ request_logs.status
type RequestStatus string

const (
	StatusPending    RequestStatus = "pending"
	StatusSuccess    RequestStatus = "success"
	StatusFailed     RequestStatus = "failed"
	StatusNoUpstream RequestStatus = "no_upstream"
	StatusTimeout    RequestStatus = "timeout"
	StatusExpired    RequestStatus = "expired"
)
```

สร้าง `internal/model/group.go`:

```go
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"time"
)

// URLSpec คือ 1 แถวใน message_group_url ที่ is_active = true
type URLSpec struct {
	ID  int64
	URL string
}

// GroupSpec คือ 1 แถวใน message_group พร้อม url ที่ active ของมัน
// เป็นหน่วยที่ reconciler ใช้ตัดสินใจทั้งหมด
type GroupSpec struct {
	ID              string // uuid ของ message_group
	Name            string
	WorkerCount     int
	UpstreamTimeout time.Duration
	RPCTimeout      time.Duration
	RefField        string
	URLs            []URLSpec
}

var (
	groupNameRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	reservedNames = map[string]bool{"healthz": true, "readyz": true}
)

// ValidateName ตรวจว่าชื่อใช้เป็นทั้งชื่อ queue และ URL path ได้
// reconciler เรียกก่อนสร้าง flow — ชื่อที่ไม่ผ่านจะถูกข้ามพร้อม log ไม่ทำให้ loop ตาย
func (g GroupSpec) ValidateName() error {
	if !groupNameRe.MatchString(g.Name) {
		return fmt.Errorf("group_name %q ไม่ตรงรูปแบบ ^[a-z0-9][a-z0-9_-]{0,63}$", g.Name)
	}
	if reservedNames[g.Name] {
		return fmt.Errorf("group_name %q เป็นคำสงวน", g.Name)
	}
	return nil
}

// QueueName คือชื่อ queue จริงบน broker — prefix กันชนกับระบบเก่าที่ใช้ broker เดียวกัน
func (g GroupSpec) QueueName(prefix string) string { return prefix + g.Name }

// HasUpstream บอกว่า group นี้ทำงานได้จริงไหม (มี url ที่ active อย่างน้อย 1 ตัว)
func (g GroupSpec) HasUpstream() bool { return len(g.URLs) > 0 }

// Revision คือลายนิ้วมือของ spec ทั้งก้อน ใช้ให้ reconciler ตัดสินได้เร็วว่าเปลี่ยนไหม
// ไม่ขึ้นกับลำดับของ URLs เพื่อให้ผลของ query ที่เรียงต่างกันไม่ทำให้ restart เปล่า ๆ
func (g GroupSpec) Revision() string {
	urls := make([]URLSpec, len(g.URLs))
	copy(urls, g.URLs)
	sort.Slice(urls, func(i, j int) bool { return urls[i].ID < urls[j].ID })

	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%d|%d|%d|%s|",
		g.ID, g.Name, g.WorkerCount,
		g.UpstreamTimeout, g.RPCTimeout, g.RefField)
	for _, u := range urls {
		fmt.Fprintf(h, "%d=%s;", u.ID, u.URL)
	}
	return hex.EncodeToString(h.Sum(nil))
}
```

- [ ] **Step 6: รัน test ให้ผ่าน**

Run: `go test ./internal/model/ -v`
Expected: PASS ทุกเคส

- [ ] **Step 7: Commit**

```bash
git add -A go.mod go.sum internal/model/ udpsocket/
git commit -m "chore: ลบ udpsocket ที่ทำให้ build พัง และเพิ่ม internal/model"
```

---

## Task 2: Config — อ่าน env แล้ว fail ทันทีถ้าตั้งผิด

**Files:**
- Create: `internal/config/config.go`, `internal/config/config_test.go`

**Interfaces:**
- Consumes: ไม่มี
- Produces: `config.Config` (struct ตามด้านล่าง), `config.Load(getenv func(string) string) (*Config, error)`

รับ `getenv` เป็น parameter ไม่เรียก `os.Getenv` ตรง ๆ เพื่อให้ test ไม่ต้องยุ่งกับ environment จริง `cmd/gateway` จะส่ง `os.Getenv` เข้ามา

- [ ] **Step 1: เขียน test ที่ยังไม่ผ่าน**

สร้าง `internal/config/config_test.go`:

```go
package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func validBase() map[string]string {
	return map[string]string{
		"RABBITMQ_URL": "amqp://guest:guest@localhost:5672/",
		"DATABASE_URL": "host=localhost user=postgres dbname=mqv2",
		"QUEUE_PREFIX": "v2.",
		"ALLOWED_IPS":  "1.2.3.4",
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	c, err := Load(env(validBase()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Port != "4000" {
		t.Errorf("Port = %q, want 4000", c.Port)
	}
	if c.ReconcileInterval != 30*time.Second {
		t.Errorf("ReconcileInterval = %v, want 30s", c.ReconcileInterval)
	}
	if c.RPCChannelPool != 4 {
		t.Errorf("RPCChannelPool = %d, want 4", c.RPCChannelPool)
	}
	if c.GracefulTimeout != 45*time.Second {
		t.Errorf("GracefulTimeout = %v, want 45s", c.GracefulTimeout)
	}
	if c.TrustedProxyCount != 1 {
		t.Errorf("TrustedProxyCount = %d, want 1", c.TrustedProxyCount)
	}
	if !c.MigrateOnStart {
		t.Error("MigrateOnStart ต้องเป็น true โดย default")
	}
}

func TestLoadRequiresEachMandatoryVarAndNamesIt(t *testing.T) {
	for _, key := range []string{"RABBITMQ_URL", "DATABASE_URL", "QUEUE_PREFIX"} {
		m := validBase()
		delete(m, key)
		_, err := Load(env(m))
		if err == nil {
			t.Fatalf("ไม่มี %s แล้วต้อง error", key)
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error ต้องบอกชื่อตัวแปร %s แต่ได้ %q", key, err.Error())
		}
	}
}

func TestQueuePrefixWhitespaceOnlyIsRejected(t *testing.T) {
	m := validBase()
	m["QUEUE_PREFIX"] = "   "
	if _, err := Load(env(m)); err == nil {
		t.Fatal("QUEUE_PREFIX ที่มีแต่ช่องว่างต้องไม่ผ่าน ไม่งั้นจะชน queue ของระบบเก่า")
	}
}

func TestAllowedIPsEmptyMeansDenyAll(t *testing.T) {
	m := validBase()
	m["ALLOWED_IPS"] = ""
	c, err := Load(env(m))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AllowAllIPs {
		t.Error("ค่าว่างต้องไม่ใช่การเปิดให้ทุกคน")
	}
	if len(c.AllowedIPs) != 0 {
		t.Errorf("AllowedIPs = %v, want ว่าง", c.AllowedIPs)
	}
}

func TestAllowedIPsStarMeansAllowAll(t *testing.T) {
	m := validBase()
	m["ALLOWED_IPS"] = "*"
	c, _ := Load(env(m))
	if !c.AllowAllIPs {
		t.Error(`ALLOWED_IPS="*" ต้องเปิดให้ทุกคน`)
	}
}

func TestAllowedIPsTrimsSpaces(t *testing.T) {
	m := validBase()
	m["ALLOWED_IPS"] = " 1.2.3.4 , 5.6.7.8 ,, "
	c, _ := Load(env(m))
	want := []string{"1.2.3.4", "5.6.7.8"}
	if len(c.AllowedIPs) != len(want) {
		t.Fatalf("AllowedIPs = %v, want %v", c.AllowedIPs, want)
	}
	for i := range want {
		if c.AllowedIPs[i] != want[i] {
			t.Errorf("AllowedIPs[%d] = %q, want %q", i, c.AllowedIPs[i], want[i])
		}
	}
}

func TestInvalidDurationIsRejected(t *testing.T) {
	m := validBase()
	m["RECONCILE_INTERVAL"] = "สามสิบวิ"
	_, err := Load(env(m))
	if err == nil || !strings.Contains(err.Error(), "RECONCILE_INTERVAL") {
		t.Fatalf("ต้อง error พร้อมบอกชื่อ RECONCILE_INTERVAL แต่ได้ %v", err)
	}
}

func TestNonPositiveNumbersRejected(t *testing.T) {
	for key, bad := range map[string]string{
		"RPC_CHANNEL_POOL":    "0",
		"TRUSTED_PROXY_COUNT": "-1",
	} {
		m := validBase()
		m[key] = bad
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%s=%s ต้องไม่ผ่าน", key, bad)
		}
	}
}

func TestMigrateOnStartCanBeDisabled(t *testing.T) {
	m := validBase()
	m["MIGRATE_ON_START"] = "false"
	c, _ := Load(env(m))
	if c.MigrateOnStart {
		t.Error("MIGRATE_ON_START=false ต้องปิด migration")
	}
}
```

- [ ] **Step 2: รัน test ให้เห็นว่า fail**

Run: `go test ./internal/config/ -v`
Expected: FAIL — คอมไพล์ไม่ผ่านเพราะยังไม่มี `Load`

- [ ] **Step 3: เขียน implementation**

สร้าง `internal/config/config.go`:

```go
package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	RabbitMQURL string
	DatabaseURL string
	QueuePrefix string

	AllowedIPs  []string
	AllowAllIPs bool

	Port              string
	ReconcileInterval time.Duration
	RPCChannelPool    int
	GracefulTimeout   time.Duration
	TrustedProxyCount int
	MigrateOnStart    bool
}

// Load อ่าน config จาก getenv แล้วคืน error ทันทีถ้าตั้งผิด
// ตั้งใจให้ process ตายตั้งแต่ตอน start ไม่ใช่ไปพังตอน runtime แบบระบบเก่า
// ที่ strconv.Atoi("") ล้มกลางทางแล้วฆ่าทั้ง container
func Load(getenv func(string) string) (*Config, error) {
	c := &Config{}
	var errs []string

	req := func(key string) string {
		v := strings.TrimSpace(getenv(key))
		if v == "" {
			errs = append(errs, fmt.Sprintf("%s ต้องตั้งค่า", key))
		}
		return v
	}

	c.RabbitMQURL = req("RABBITMQ_URL")
	c.DatabaseURL = req("DATABASE_URL")
	// QUEUE_PREFIX กันไม่ให้ service ใหม่ไปแย่งกิน queue ของระบบเก่าที่ใช้ broker เดียวกัน
	c.QueuePrefix = req("QUEUE_PREFIX")

	raw := strings.TrimSpace(getenv("ALLOWED_IPS"))
	if raw == "*" {
		c.AllowAllIPs = true
	} else {
		for _, p := range strings.Split(raw, ",") {
			if p = strings.TrimSpace(p); p != "" {
				c.AllowedIPs = append(c.AllowedIPs, p)
			}
		}
	}

	c.Port = str(getenv, "PORT", "4000")
	c.MigrateOnStart = boolean(getenv, "MIGRATE_ON_START", true)

	c.ReconcileInterval = duration(getenv, "RECONCILE_INTERVAL", 30*time.Second, &errs)
	c.GracefulTimeout = duration(getenv, "GRACEFUL_TIMEOUT", 45*time.Second, &errs)
	c.RPCChannelPool = positiveInt(getenv, "RPC_CHANNEL_POOL", 4, &errs)
	c.TrustedProxyCount = nonNegativeInt(getenv, "TRUSTED_PROXY_COUNT", 1, &errs)

	if len(errs) > 0 {
		return nil, fmt.Errorf("config ไม่ถูกต้อง: %s", strings.Join(errs, "; "))
	}
	return c, nil
}

func str(getenv func(string) string, key, def string) string {
	if v := strings.TrimSpace(getenv(key)); v != "" {
		return v
	}
	return def
}

func boolean(getenv func(string) string, key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(getenv(key)))
	if v == "" {
		return def
	}
	return v != "false" && v != "0" && v != "no"
}

func duration(getenv func(string) string, key string, def time.Duration, errs *[]string) time.Duration {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s อ่านไม่ได้: %v", key, err))
		return def
	}
	if d <= 0 {
		*errs = append(*errs, fmt.Sprintf("%s ต้องมากกว่า 0", key))
		return def
	}
	return d
}

func positiveInt(getenv func(string) string, key string, def int, errs *[]string) int {
	n, ok := parseInt(getenv, key, def, errs)
	if ok && n < 1 {
		*errs = append(*errs, fmt.Sprintf("%s ต้องมากกว่า 0", key))
	}
	return n
}

func nonNegativeInt(getenv func(string) string, key string, def int, errs *[]string) int {
	n, ok := parseInt(getenv, key, def, errs)
	if ok && n < 0 {
		*errs = append(*errs, fmt.Sprintf("%s ต้องไม่ติดลบ", key))
	}
	return n
}

func parseInt(getenv func(string) string, key string, def int, errs *[]string) (int, bool) {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return def, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s อ่านไม่ได้: %v", key, err))
		return def, false
	}
	return n, true
}
```

- [ ] **Step 4: รัน test ให้ผ่าน**

Run: `go test ./internal/config/ -v`
Expected: PASS ทุกเคส

- [ ] **Step 5: Commit**

```bash
git add internal/config/
git commit -m "feat(config): อ่าน env พร้อม validate และ fail ตั้งแต่ start"
```

---

## Task 3: ตารางจำแนก error — หัวใจของการกันออเดอร์ซ้ำ

**Files:**
- Create: `internal/forward/classify.go`, `internal/forward/classify_test.go`

**Interfaces:**
- Consumes: `model.Outcome` จาก Task 1
- Produces: `forward.AttemptResult{Status int, WroteRequest bool, Err error}`,
  `forward.Classify(AttemptResult) model.Outcome`

`WroteRequest` คือกุญแจ — มันมาจาก `httptrace.ClientTrace.WroteRequest` ซึ่งบอกได้จริงว่า body ถูกส่งออกไปหรือยัง ทำให้แยก "ต่อไม่ติด" (ปลอดภัยที่จะลองใหม่) ออกจาก "ส่งไปแล้วไม่รู้ผล" (ห้ามลองใหม่) ได้อย่างแม่นยำแทนที่จะเดาจากข้อความ error

- [ ] **Step 1: เขียน test ที่ยังไม่ผ่าน**

สร้าง `internal/forward/classify_test.go`:

```go
package forward

import (
	"errors"
	"testing"

	"github.com/celalsahinaltinisik/internal/model"
)

func TestClassifyMatchesSpecTable(t *testing.T) {
	someErr := errors.New("boom")
	cases := []struct {
		name string
		in   AttemptResult
		want model.Outcome
	}{
		// สำเร็จ
		{"200", AttemptResult{Status: 200}, model.OutcomeSuccess},
		{"201", AttemptResult{Status: 201}, model.OutcomeSuccess},

		// ยังไม่ได้ส่ง body ออกไป = ปลอดภัยที่จะลอง url ถัดไป
		{"ต่อไม่ติด", AttemptResult{Err: someErr, WroteRequest: false}, model.OutcomeRetryable},

		// ส่ง body ไปแล้วแต่พัง = ไม่รู้ว่าออเดอร์เกิดหรือยัง ห้ามลองใหม่
		{"พังหลังส่ง body", AttemptResult{Err: someErr, WroteRequest: true}, model.OutcomeFatal},

		// ปลายทางปฏิเสธก่อนประมวลผล
		{"404", AttemptResult{Status: 404}, model.OutcomeRetryable},
		{"429", AttemptResult{Status: 429}, model.OutcomeRetryable},
		{"502", AttemptResult{Status: 502}, model.OutcomeRetryable},
		{"503", AttemptResult{Status: 503}, model.OutcomeRetryable},

		// คำขอผิดเอง ลองกี่ทีก็ผิด
		{"400", AttemptResult{Status: 400}, model.OutcomeFatal},
		{"401", AttemptResult{Status: 401}, model.OutcomeFatal},
		{"403", AttemptResult{Status: 403}, model.OutcomeFatal},
		{"422", AttemptResult{Status: 422}, model.OutcomeFatal},

		// แอปรับคำขอไปแล้ว อาจสร้างออเดอร์ไปบางส่วน
		{"500", AttemptResult{Status: 500}, model.OutcomeFatal},
		{"504", AttemptResult{Status: 504}, model.OutcomeFatal},

		// 5xx อื่นที่ไม่ได้อยู่ในรายการ — เลือกทางปลอดภัยไว้ก่อน
		{"501", AttemptResult{Status: 501}, model.OutcomeFatal},
		{"505", AttemptResult{Status: 505}, model.OutcomeFatal},

		// 3xx ไม่ตาม redirect เอง ถือว่าตั้ง url ผิด
		{"301", AttemptResult{Status: 301}, model.OutcomeFatal},
	}
	for _, c := range cases {
		if got := Classify(c.in); got != c.want {
			t.Errorf("%s: Classify = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestErrorWinsOverStatus(t *testing.T) {
	// ถ้ามี error แปลว่าไม่มี response ที่เชื่อถือได้ ต้องดูที่ WroteRequest อย่างเดียว
	got := Classify(AttemptResult{Status: 200, Err: errors.New("reset"), WroteRequest: true})
	if got != model.OutcomeFatal {
		t.Fatalf("Classify = %q, want fatal", got)
	}
}

func TestNoStatusNoErrorIsFatal(t *testing.T) {
	// สถานะที่ไม่ควรเกิด — อย่าเงียบ ให้ถือว่า fatal จะได้เห็นใน log
	if got := Classify(AttemptResult{}); got != model.OutcomeFatal {
		t.Fatalf("Classify = %q, want fatal", got)
	}
}
```

- [ ] **Step 2: รัน test ให้เห็นว่า fail**

Run: `go test ./internal/forward/ -run Classify -v`
Expected: FAIL — ยังไม่มี `Classify`

- [ ] **Step 3: เขียน implementation**

สร้าง `internal/forward/classify.go`:

```go
package forward

import "github.com/celalsahinaltinisik/internal/model"

// AttemptResult คือผลดิบของการยิง upstream หนึ่งครั้ง ก่อนถูกจำแนก
type AttemptResult struct {
	Status int
	// WroteRequest มาจาก httptrace บอกว่า request ถูกเขียนออก socket ครบแล้วหรือยัง
	// เป็นตัวชี้ขาดว่าปลายทางมีโอกาสเห็นคำขอนี้หรือไม่
	WroteRequest bool
	Err          error
}

// Classify ตัดสินว่าจะลอง url ถัดไปได้หรือไม่
//
// กติกาเดียว: retryable ได้เฉพาะเมื่อมั่นใจว่าคำขอไปไม่ถึงแอปปลายทาง
// เพราะนี่เป็นงานการเงิน การลองซ้ำผิดจังหวะ = ถอนเงินหรือสร้าง QR ซ้ำให้ลูกค้าจริง
// ห้ามทำให้ตารางนี้ config ได้ — เป็นความถูกต้องทางธุรกิจ ไม่ใช่การปรับจูน
func Classify(r AttemptResult) model.Outcome {
	if r.Err != nil {
		if r.WroteRequest {
			// ส่ง body ออกไปแล้วถึงพัง (timeout รอ response, connection reset)
			// แอปปลายทางอาจประมวลผลไปแล้ว ลองใหม่ = เสี่ยงซ้ำ
			return model.OutcomeFatal
		}
		// ยังเขียน request ไม่เสร็จ (DNS fail, connection refused, TLS handshake fail)
		// แอปปลายทางไม่มีทางเห็นคำขอนี้ ลอง url ถัดไปได้
		return model.OutcomeRetryable
	}

	switch {
	case r.Status >= 200 && r.Status < 300:
		return model.OutcomeSuccess
	case r.Status == 404, r.Status == 429, r.Status == 502, r.Status == 503:
		// ถูกปฏิเสธหรือต่อ backend ไม่ได้ตั้งแต่ชั้น proxy — แอปยังไม่ได้ประมวลผล
		return model.OutcomeRetryable
	default:
		// รวม 400/401/403/422 (คำขอผิดเอง)
		// และ 500/504 กับ 5xx อื่น (แอปรับไปแล้ว อาจสร้างออเดอร์ไปบางส่วน)
		// และ 3xx (ตั้ง url ผิด ไม่ตาม redirect เอง)
		return model.OutcomeFatal
	}
}
```

- [ ] **Step 4: รัน test ให้ผ่าน**

Run: `go test ./internal/forward/ -run Classify -v`
Expected: PASS ทุกเคส

- [ ] **Step 5: Commit**

```bash
git add internal/forward/
git commit -m "feat(forward): ตารางจำแนก error สำหรับตัดสินว่า retry ได้หรือไม่"
```

---

## Task 4: Forwarder — ยิง upstream ตามลำดับพร้อม budget เวลา

**Files:**
- Create: `internal/forward/forwarder.go`, `internal/forward/forwarder_test.go`

**Interfaces:**
- Consumes: `model.GroupSpec`, `model.URLSpec`, `forward.Classify`
- Produces:
  - `forward.Attempt{Seq int, URLID int64, URL string, HTTPStatus int, Duration time.Duration, Outcome model.Outcome, Body []byte, ErrMessage string}`
  - `forward.Result{Attempts []Attempt, Final *Attempt}` — `Final` เป็น nil เมื่อไม่มี url ให้ยิงเลย
  - `forward.New() *Forwarder`
  - `(*Forwarder).Send(ctx context.Context, spec model.GroupSpec, body []byte, hdr http.Header, deadline time.Time) Result`
  - field ที่ test override ได้: `Shuffle func([]model.URLSpec)`, `Now func() time.Time`, `MinAttemptBudget time.Duration`, `MaxResponseBytes int64`

**เบี่ยงจาก spec §7.4 อย่างตั้งใจ:** spec เขียนว่า "เวลาที่เหลือ < upstream_timeout → หยุด" ถ้าทำตามตัวอักษร request ที่เหลือเวลา 25 วินาทีจะไม่ถูกยิงเลยทั้งที่ upstream ปกติตอบใน 1 วินาที แผนนี้ใช้ `timeout = min(upstream_timeout, เวลาที่เหลือ)` และหยุดก็ต่อเมื่อเหลือน้อยกว่า `MinAttemptBudget` (default 1s) ซึ่งรักษาเจตนาเดิม (ไม่เริ่มสิ่งที่รู้ว่าไม่ทัน) โดยไม่ทิ้งเวลาที่ยังใช้ได้

- [ ] **Step 1: เขียน test ที่ยังไม่ผ่าน**

สร้าง `internal/forward/forwarder_test.go`:

```go
package forward

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/celalsahinaltinisik/internal/model"
)

// newTestForwarder ปิดการสุ่มเพื่อให้ลำดับ url แน่นอนในการทดสอบ
func newTestForwarder() *Forwarder {
	f := New()
	f.Shuffle = func([]model.URLSpec) {}
	return f
}

func specWith(urls ...string) model.GroupSpec {
	g := model.GroupSpec{
		Name:            "withdraw",
		UpstreamTimeout: 2 * time.Second,
		RPCTimeout:      10 * time.Second,
	}
	for i, u := range urls {
		g.URLs = append(g.URLs, model.URLSpec{ID: int64(i + 1), URL: u})
	}
	return g
}

func farDeadline() time.Time { return time.Now().Add(time.Minute) }

func TestSendStopsAtFirstSuccess(t *testing.T) {
	hits := 0
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer ok.Close()

	res := newTestForwarder().Send(context.Background(),
		specWith(ok.URL, ok.URL), []byte(`{}`), http.Header{}, farDeadline())

	if len(res.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(res.Attempts))
	}
	if hits != 1 {
		t.Errorf("ยิง upstream %d ครั้ง, want 1", hits)
	}
	if res.Final == nil || res.Final.Outcome != model.OutcomeSuccess {
		t.Fatalf("Final = %+v, want success", res.Final)
	}
	if string(res.Final.Body) != `{"code":0}` {
		t.Errorf("Body = %q", res.Final.Body)
	}
}

func TestSendFailsOverOnRetryableThenSucceeds(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer good.Close()

	res := newTestForwarder().Send(context.Background(),
		specWith(bad.URL, good.URL), []byte(`{}`), http.Header{}, farDeadline())

	if len(res.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(res.Attempts))
	}
	if res.Attempts[0].Seq != 1 || res.Attempts[1].Seq != 2 {
		t.Errorf("seq ต้องเป็น 1,2 แต่ได้ %d,%d", res.Attempts[0].Seq, res.Attempts[1].Seq)
	}
	if res.Attempts[0].Outcome != model.OutcomeRetryable {
		t.Errorf("attempt 1 = %q, want retryable", res.Attempts[0].Outcome)
	}
	if res.Final.Outcome != model.OutcomeSuccess {
		t.Errorf("Final = %q, want success", res.Final.Outcome)
	}
}

func TestSendStopsOnFatalWithoutTryingNextURL(t *testing.T) {
	secondHit := false
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"message":"ยอดเงินไม่พอ"}`))
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHit = true
		w.WriteHeader(200)
	}))
	defer second.Close()

	res := newTestForwarder().Send(context.Background(),
		specWith(first.URL, second.URL), []byte(`{}`), http.Header{}, farDeadline())

	if len(res.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(res.Attempts))
	}
	if secondHit {
		t.Fatal("400 คือคำขอผิดเอง ห้ามลอง url ถัดไป")
	}
	if res.Final.Outcome != model.OutcomeFatal {
		t.Errorf("Final = %q, want fatal", res.Final.Outcome)
	}
}

func TestTimeoutAfterRequestSentIsFatalNotRetried(t *testing.T) {
	secondHit := false
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer slow.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHit = true
	}))
	defer second.Close()

	spec := specWith(slow.URL, second.URL)
	spec.UpstreamTimeout = 100 * time.Millisecond // สั้นกว่าที่ server ใช้ตอบ

	res := newTestForwarder().Send(context.Background(),
		spec, []byte(`{}`), http.Header{}, farDeadline())

	if secondHit {
		t.Fatal("timeout หลังส่ง body แล้ว ห้ามลอง url ถัดไป เพราะออเดอร์อาจเกิดแล้ว")
	}
	if res.Final.Outcome != model.OutcomeFatal {
		t.Fatalf("Final = %q, want fatal", res.Final.Outcome)
	}
}

func TestConnectionRefusedIsRetryable(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // ปิดทิ้งเพื่อให้ต่อไม่ติด

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer good.Close()

	res := newTestForwarder().Send(context.Background(),
		specWith(deadURL, good.URL), []byte(`{}`), http.Header{}, farDeadline())

	if len(res.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2 (ต่อไม่ติดต้องลองตัวถัดไป)", len(res.Attempts))
	}
	if res.Attempts[0].Outcome != model.OutcomeRetryable {
		t.Errorf("attempt 1 = %q, want retryable", res.Attempts[0].Outcome)
	}
	if res.Final.Outcome != model.OutcomeSuccess {
		t.Errorf("Final = %q, want success", res.Final.Outcome)
	}
}

// Review Focus #4 — url ผิดรูปใน DB ต้องไม่ทำให้ panic
func TestMalformedURLIsFatalAttemptNotPanic(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer good.Close()

	for _, bad := range []string{"", "   ", "not-a-url", "http://[::1]:namedport"} {
		res := newTestForwarder().Send(context.Background(),
			specWith(bad), []byte(`{}`), http.Header{}, farDeadline())
		if len(res.Attempts) != 1 {
			t.Fatalf("url %q: attempts = %d, want 1", bad, len(res.Attempts))
		}
		if res.Attempts[0].Outcome != model.OutcomeFatal {
			t.Errorf("url %q: outcome = %q, want fatal", bad, res.Attempts[0].Outcome)
		}
		if res.Attempts[0].ErrMessage == "" {
			t.Errorf("url %q: ต้องมี ErrMessage ไว้ให้ไล่ปัญหา", bad)
		}
	}
}

func TestNoActiveURLReturnsEmptyResult(t *testing.T) {
	res := newTestForwarder().Send(context.Background(),
		specWith(), []byte(`{}`), http.Header{}, farDeadline())
	if len(res.Attempts) != 0 || res.Final != nil {
		t.Fatalf("group ที่ไม่มี url ต้องได้ผลว่าง แต่ได้ %+v", res)
	}
}

func TestDeadlineAlreadyPassedMeansNoAttempt(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
	}))
	defer srv.Close()

	res := newTestForwarder().Send(context.Background(),
		specWith(srv.URL), []byte(`{}`), http.Header{}, time.Now().Add(-time.Second))

	if hit {
		t.Fatal("deadline ผ่านไปแล้ว ห้ามยิง upstream")
	}
	if len(res.Attempts) != 0 {
		t.Fatalf("attempts = %d, want 0", len(res.Attempts))
	}
}

func TestStopsWhenRemainingBudgetTooSmallForAnotherAttempt(t *testing.T) {
	secondHit := false
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHit = true
	}))
	defer second.Close()

	f := newTestForwarder()
	f.MinAttemptBudget = time.Hour // ทำให้ไม่เหลือ budget พอสำหรับครั้งที่สองแน่ ๆ

	res := f.Send(context.Background(),
		specWith(first.URL, second.URL), []byte(`{}`), http.Header{}, time.Now().Add(2*time.Second))

	if secondHit {
		t.Fatal("เวลาไม่พอแล้ว ห้ามเริ่ม attempt ใหม่")
	}
	if len(res.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(res.Attempts))
	}
}

func TestForwardsCallerHeadersButStripsHopByHop(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		got.Set("Host", r.Host)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer abc123")
	hdr.Set("X-Merchant-Id", "M001")
	hdr.Set("Connection", "keep-alive")
	hdr.Set("Host", "evil.example.com")

	newTestForwarder().Send(context.Background(),
		specWith(srv.URL), []byte(`{}`), hdr, farDeadline())

	if got.Get("Authorization") != "Bearer abc123" {
		t.Error("Authorization ต้องถูกส่งต่อ")
	}
	if got.Get("X-Merchant-Id") != "M001" {
		t.Error("header ของ caller ตัวอื่นต้องถูกส่งต่อ")
	}
	if got.Get("Connection") == "keep-alive" {
		t.Error("Connection เป็น hop-by-hop ต้องไม่ถูกส่งต่อ")
	}
	if got.Get("Host") == "evil.example.com" {
		t.Error("Host ของ caller ต้องไม่ถูกส่งต่อไป upstream")
	}
	if got.Get("Content-Type") != "application/json" {
		t.Error("ต้องตั้ง Content-Type เป็น application/json")
	}
}

func TestOversizedResponseIsFatalNotTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		blob := make([]byte, 4096)
		_, _ = w.Write(blob)
	}))
	defer srv.Close()

	f := newTestForwarder()
	f.MaxResponseBytes = 1024

	res := f.Send(context.Background(),
		specWith(srv.URL), []byte(`{}`), http.Header{}, farDeadline())

	if res.Final.Outcome != model.OutcomeFatal {
		t.Fatalf("Final = %q, want fatal — response ใหญ่เกินต้องไม่ถูกตัดแล้วส่งต่อเงียบ ๆ",
			res.Final.Outcome)
	}
}

func TestAttemptRecordsURLIDAndDuration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	res := newTestForwarder().Send(context.Background(),
		specWith(srv.URL), []byte(`{}`), http.Header{}, farDeadline())

	a := res.Attempts[0]
	if a.URLID != 1 {
		t.Errorf("URLID = %d, want 1 — ต้องผูกกลับไปหาแถวใน message_group_url ได้", a.URLID)
	}
	if a.URL != srv.URL {
		t.Errorf("URL = %q, want %q", a.URL, srv.URL)
	}
	if a.Duration <= 0 {
		t.Error("Duration ต้องถูกบันทึก")
	}
}
```

- [ ] **Step 2: รัน test ให้เห็นว่า fail**

Run: `go test ./internal/forward/ -run 'TestSend|TestTimeout|TestConnection|TestMalformed|TestNoActive|TestDeadline|TestStops|TestForwards|TestOversized|TestAttempt' -v`
Expected: FAIL — ยังไม่มี `New` และ `Send`

- [ ] **Step 3: เขียน implementation**

สร้าง `internal/forward/forwarder.go`:

```go
package forward

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"

	"github.com/celalsahinaltinisik/internal/model"
)

// Attempt คือบันทึกของการยิง upstream หนึ่งครั้ง ตรงกับหนึ่งแถวใน attempt_logs
type Attempt struct {
	Seq        int
	URLID      int64
	URL        string
	HTTPStatus int
	Duration   time.Duration
	Outcome    model.Outcome
	Body       []byte
	ErrMessage string
}

// Result คือผลรวมของการพยายามทั้งหมดสำหรับหนึ่ง request
// Final เป็น nil เมื่อไม่มี url ให้ยิงเลย (caller ต้องแปลเป็น no_upstream)
type Result struct {
	Attempts []Attempt
	Final    *Attempt
}

type Forwarder struct {
	Client           *http.Client
	Shuffle          func([]model.URLSpec)
	Now              func() time.Time
	MinAttemptBudget time.Duration
	MaxResponseBytes int64
}

func New() *Forwarder {
	return &Forwarder{
		// ไม่ตั้ง Timeout ที่ Client เพราะคุมด้วย context ต่อ attempt แทน
		Client:           &http.Client{},
		Shuffle:          func(u []model.URLSpec) { rand.Shuffle(len(u), func(i, j int) { u[i], u[j] = u[j], u[i] }) },
		Now:              time.Now,
		MinAttemptBudget: time.Second,
		MaxResponseBytes: 8 * 1024 * 1024,
	}
}

var hopByHop = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Host":                true,
	"Content-Length":      true,
}

// Send ยิง upstream ทีละ url จนกว่าจะสำเร็จ เจอ fatal หรือหมดเวลา
// deadline มาจาก header x-deadline ของข้อความ ซึ่งคำนวณตอนรับ HTTP เข้ามา
// ทำให้ worker ไม่มีทางยิง upstream หลังจาก caller เลิกรอไปแล้ว
func (f *Forwarder) Send(ctx context.Context, spec model.GroupSpec, body []byte,
	hdr http.Header, deadline time.Time) Result {

	urls := make([]model.URLSpec, len(spec.URLs))
	copy(urls, spec.URLs)
	f.Shuffle(urls)

	res := Result{}
	for i, u := range urls {
		remaining := deadline.Sub(f.Now())
		if remaining <= 0 {
			break
		}
		// ไม่เริ่ม attempt ใหม่ที่เวลาเหลือน้อยจนไม่น่าจะทัน
		if i > 0 && remaining < f.MinAttemptBudget {
			break
		}
		timeout := spec.UpstreamTimeout
		if timeout > remaining {
			timeout = remaining
		}

		a := f.attempt(ctx, i+1, u, body, hdr, timeout)
		res.Attempts = append(res.Attempts, a)
		res.Final = &res.Attempts[len(res.Attempts)-1]

		if a.Outcome != model.OutcomeRetryable {
			break
		}
	}
	return res
}

func (f *Forwarder) attempt(ctx context.Context, seq int, u model.URLSpec, body []byte,
	hdr http.Header, timeout time.Duration) Attempt {

	a := Attempt{Seq: seq, URLID: u.ID, URL: u.URL}
	start := f.Now()

	rawURL := strings.TrimSpace(u.URL)
	if rawURL == "" || !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		a.Outcome = model.OutcomeFatal
		a.ErrMessage = fmt.Sprintf("url ไม่ถูกต้อง: %q", u.URL)
		a.Duration = f.Now().Sub(start)
		return a
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		a.Outcome = model.OutcomeFatal
		a.ErrMessage = fmt.Sprintf("สร้าง request ไม่ได้: %v", err)
		a.Duration = f.Now().Sub(start)
		return a
	}

	for k, vs := range hdr {
		if hopByHop[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")

	// wrote บอกว่า body ถูกเขียนออก socket ครบหรือยัง เป็นตัวชี้ขาดว่า retry ได้ไหม
	var wrote atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				wrote.Store(true)
			}
		},
	}))

	resp, err := f.Client.Do(req)
	if err != nil {
		a.Duration = f.Now().Sub(start)
		a.ErrMessage = err.Error()
		a.Outcome = Classify(AttemptResult{Err: err, WroteRequest: wrote.Load()})
		return a
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, f.MaxResponseBytes+1)
	raw, readErr := io.ReadAll(limited)
	a.Duration = f.Now().Sub(start)
	a.HTTPStatus = resp.StatusCode

	if readErr != nil {
		a.ErrMessage = fmt.Sprintf("อ่าน response ไม่สำเร็จ: %v", readErr)
		a.Outcome = Classify(AttemptResult{Err: readErr, WroteRequest: true})
		return a
	}
	if int64(len(raw)) > f.MaxResponseBytes {
		// ไม่ตัดแล้วส่งต่อ เพราะ response ที่ถูกตัดคือ JSON พังที่ caller แปลไม่ออก
		a.ErrMessage = fmt.Sprintf("response ใหญ่เกิน %d ไบต์", f.MaxResponseBytes)
		a.Outcome = Classify(AttemptResult{Err: errors.New("response too large"), WroteRequest: true})
		return a
	}

	a.Body = raw
	a.Outcome = Classify(AttemptResult{Status: resp.StatusCode, WroteRequest: wrote.Load()})
	return a
}
```

- [ ] **Step 4: รัน test ให้ผ่าน**

Run: `go test ./internal/forward/ -v`
Expected: PASS ทุกเคส รวม classify จาก Task 3 ด้วย

- [ ] **Step 5: Commit**

```bash
git add internal/forward/
git commit -m "feat(forward): ยิง upstream ตามลำดับพร้อม failover และ budget เวลา"
```

---

## แนวทางการทดสอบส่วนที่ต้องใช้ Postgres / RabbitMQ

เครื่อง dev **ไม่มี docker** ใช้ testcontainers ไม่ได้ Task 5-8 จึงใช้ integration test ที่

- อยู่หลัง build tag `//go:build integration` — `go test ./...` ปกติจะไม่แตะเลย
- `t.Skip` เองถ้าไม่มี `TEST_DATABASE_URL` / `TEST_RABBITMQ_URL`
- สร้าง schema ชั่วคราวของตัวเองต่อการรันหนึ่งครั้ง แล้ว drop ทิ้ง — ไม่แตะตารางจริง

รันด้วย:

```bash
export TEST_DATABASE_URL='host=... user=... password=... dbname=mqdev port=... sslmode=disable'
export TEST_RABBITMQ_URL='amqp://guest:guest@...:5672/'
go test -tags=integration ./... -v
```

---

## Task 5: Migration ที่รันเองตอน start

**Files:**
- Create: `internal/store/migrations/0001_init.sql`
- Create: `internal/store/migrate.go`, `internal/store/db.go`
- Create: `internal/store/testsupport_test.go`, `internal/store/migrate_test.go`

**Interfaces:**
- Consumes: ไม่มี
- Produces: `store.Open(dsn string) (*sql.DB, error)`, `store.Migrate(ctx context.Context, db *sql.DB) error`

- [ ] **Step 1: เขียนไฟล์ schema**

สร้าง `internal/store/migrations/0001_init.sql` — ตรงกับ spec §4 ทุกคอลัมน์:

```sql
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS message_group (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    group_name          TEXT NOT NULL UNIQUE,
    worker_count        INT  NOT NULL DEFAULT 50,
    upstream_timeout_ms INT  NOT NULL DEFAULT 30000,
    rpc_timeout_ms      INT  NOT NULL DEFAULT 60000,
    ref_field           TEXT NOT NULL DEFAULT 'customer_order_id',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS message_group_url (
    id               BIGSERIAL PRIMARY KEY,
    message_group_id UUID NOT NULL REFERENCES message_group(id) ON DELETE CASCADE,
    url              TEXT NOT NULL,
    is_active        BOOLEAN NOT NULL DEFAULT true,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (message_group_id, url)
);

CREATE INDEX IF NOT EXISTS idx_message_group_url_parent
    ON message_group_url (message_group_id) WHERE is_active;

CREATE TABLE IF NOT EXISTS request_logs (
    trace_id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    message_group_id UUID,
    group_name       TEXT NOT NULL,
    status           TEXT NOT NULL,
    business_ref     TEXT,
    caller_trace_id  TEXT,
    client_ip        TEXT,
    request_body     JSONB,
    response_body    JSONB,
    http_status      INT,
    attempt_count    INT NOT NULL DEFAULT 0,
    total_ms         INT,
    error_message    TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at      TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS attempt_logs (
    id                   BIGSERIAL PRIMARY KEY,
    trace_id             UUID NOT NULL REFERENCES request_logs(trace_id) ON DELETE CASCADE,
    seq                  INT  NOT NULL,
    message_group_url_id BIGINT,
    url                  TEXT NOT NULL,
    http_status          INT,
    duration_ms          INT,
    outcome              TEXT NOT NULL,
    response_body        TEXT,
    error_message        TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (trace_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_request_logs_pending
    ON request_logs (created_at) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_request_logs_ref
    ON request_logs (business_ref) WHERE business_ref IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_request_logs_group
    ON request_logs (message_group_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_attempt_logs_url
    ON attempt_logs (message_group_url_id, created_at DESC);
```

- [ ] **Step 2: เขียน test helper และ test ที่ยังไม่ผ่าน**

สร้าง `internal/store/testsupport_test.go`:

```go
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
```

สร้าง `internal/store/migrate_test.go`:

```go
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
	for _, name := range []string{"message_group", "message_group_url", "request_logs", "attempt_logs", "schema_migrations"} {
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
```

- [ ] **Step 3: รัน test ให้เห็นว่า fail**

Run: `go test -tags=integration ./internal/store/ -v`
Expected: FAIL — ยังไม่มี `Migrate` (ถ้าไม่มี `TEST_DATABASE_URL` จะขึ้น SKIP ซึ่งยังไม่พอ ต้องตั้ง env ก่อน)

- [ ] **Step 4: เขียน implementation**

สร้าง `internal/store/db.go`:

```go
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
```

สร้าง `internal/store/migrate.go`:

```go
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
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
```

- [ ] **Step 5: รัน test ให้ผ่าน**

Run: `go test -tags=integration ./internal/store/ -v`
Expected: PASS ทั้งสองเคส

- [ ] **Step 6: ยืนยันว่า unit test ปกติยังไม่ต้องใช้ DB**

Run: `go test ./...`
Expected: PASS และ `internal/store` ขึ้น `[no test files]` เพราะ test ทั้งหมดอยู่หลัง build tag

- [ ] **Step 7: Commit**

```bash
git add internal/store/
git commit -m "feat(store): migration ฝังใน binary พร้อม advisory lock"
```

---

## Task 6: อ่าน desired state จาก DB

**Files:**
- Create: `internal/store/groups.go`, `internal/store/groups_test.go`

**Interfaces:**
- Consumes: `store.Migrate`, `model.GroupSpec`, `model.URLSpec`
- Produces: `store.New(db *sql.DB) *Store`, `(*Store).LoadGroups(ctx context.Context) ([]model.GroupSpec, error)`

`LoadGroups` คืน group ที่ **ไม่มี url ที่ active ด้วย** (URLs ว่าง) เพื่อให้ reconciler ตัดสินใจเองว่าจะ mark เป็น degraded — ไม่ใช่หน้าที่ของ store ที่จะซ่อนมัน

- [ ] **Step 1: เขียน test ที่ยังไม่ผ่าน**

สร้าง `internal/store/groups_test.go`:

```go
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
```

- [ ] **Step 2: รัน test ให้เห็นว่า fail**

Run: `go test -tags=integration ./internal/store/ -run LoadGroups -v`
Expected: FAIL — ยังไม่มี `New` และ `LoadGroups`

- [ ] **Step 3: เขียน implementation**

สร้าง `internal/store/groups.go`:

```go
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
			id, name, refField       string
			workers, upMS, rpcMS     int
			urlID                    sql.NullInt64
			urlStr                   sql.NullString
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
```

- [ ] **Step 4: รัน test ให้ผ่าน**

Run: `go test -tags=integration ./internal/store/ -v`
Expected: PASS ทุกเคส

- [ ] **Step 5: Commit**

```bash
git add internal/store/
git commit -m "feat(store): อ่าน desired state ของ group ทั้งระบบด้วย query เดียว"
```

---

## Task 7: เขียน log ที่ตาม request ได้

**Files:**
- Create: `internal/model/payload.go`, `internal/model/payload_test.go`
- Create: `internal/store/logs.go`, `internal/store/logs_test.go`

**Interfaces:**
- Consumes: `store.New`, `model.RequestStatus`, `model.Outcome`
- Produces:
  - `model.JSONOrRaw(body []byte) []byte` — คืน body เดิมถ้าเป็น JSON ที่ใช้ได้ ไม่งั้นห่อเป็น `{"raw":"..."}`
  - `model.ExtractRef(body []byte, field string) string`
  - `store.BeginRequestInput{TraceID, GroupID, GroupName, CallerTraceID, ClientIP string, Body []byte, BusinessRef string}`
  - `(*Store).BeginRequest(ctx, BeginRequestInput) error`
  - `store.AttemptInput{Seq int, URLID int64, URL string, HTTPStatus int, DurationMS int, Outcome model.Outcome, ResponseBody string, ErrMessage string}`
  - `(*Store).RecordAttempts(ctx, traceID string, attempts []AttemptInput) error`
  - `store.FinishRequestInput{TraceID string, Status model.RequestStatus, ResponseBody []byte, AttemptCount int, TotalMS int, ErrMessage string}`
  - `(*Store).FinishRequest(ctx, FinishRequestInput) error`
  - `(*Store).MarkClientOutcome(ctx, traceID string, httpStatus int) error`

**การแบ่งหน้าที่เขียนคอลัมน์ ที่กันสองฝั่งเขียนทับกัน** (spec §4.2):

| ใคร | เขียนคอลัมน์ไหน |
|---|---|
| ฝั่งรับ HTTP ตอนเริ่ม | INSERT แถว `status='pending'` |
| worker ตอนจบ | `status`, `response_body`, `attempt_count`, `total_ms`, `error_message`, `finished_at` |
| ฝั่งรับ HTTP ตอนตอบ caller | `http_status` เท่านั้น และตั้ง `status='timeout'` **ก็ต่อเมื่อยังเป็น `pending`** |

ผลคือแถวที่ `http_status=504` แต่ `status='success'` = caller คิดว่าล้มเหลวแต่ออเดอร์เกิดจริง ซึ่งเป็นเคสอันตรายที่สุดและตอนนี้ query เจอ

`RecordAttempts` เขียนทีเดียวเป็น multi-row insert หลัง `Send` จบ แทนที่จะเขียนทีละ attempt เพื่อลดจำนวน write — ความสามารถในการเห็น request ที่กำลังค้างมาจากแถว `pending` ใน `request_logs` อยู่แล้ว

- [ ] **Step 1: เขียน unit test ของ payload (ไม่ต้องใช้ DB)**

สร้าง `internal/model/payload_test.go`:

```go
package model

import "testing"

func TestJSONOrRawKeepsValidJSON(t *testing.T) {
	in := []byte(`{"amount":100}`)
	if got := string(JSONOrRaw(in)); got != `{"amount":100}` {
		t.Fatalf("JSONOrRaw = %s", got)
	}
}

func TestJSONOrRawWrapsInvalidInput(t *testing.T) {
	for _, in := range []string{"", "   ", "not json", `{"broken":`} {
		got := JSONOrRaw([]byte(in))
		if !isValidJSON(got) {
			t.Errorf("JSONOrRaw(%q) = %s ซึ่งยัง insert ลง JSONB ไม่ได้", in, got)
		}
	}
}

func TestJSONOrRawHandlesBinary(t *testing.T) {
	got := JSONOrRaw([]byte{0xff, 0xfe, 0x00})
	if !isValidJSON(got) {
		t.Fatalf("binary body ต้องถูกห่อจนเป็น JSON ที่ใช้ได้ แต่ได้ %q", got)
	}
}

func TestExtractRef(t *testing.T) {
	cases := []struct {
		body, field, want string
	}{
		{`{"customer_order_id":"ORDER-1"}`, "customer_order_id", "ORDER-1"},
		{`{"ref1":"R1","customer_order_id":"ORDER-2"}`, "ref1", "R1"},
		{`{"customer_order_id":12345}`, "customer_order_id", "12345"},
		{`{"other":"x"}`, "customer_order_id", ""},
		{`not json`, "customer_order_id", ""},
		{``, "customer_order_id", ""},
		{`{"customer_order_id":null}`, "customer_order_id", ""},
		{`{"customer_order_id":{"nested":1}}`, "customer_order_id", ""},
	}
	for _, c := range cases {
		if got := ExtractRef([]byte(c.body), c.field); got != c.want {
			t.Errorf("ExtractRef(%s, %s) = %q, want %q", c.body, c.field, got, c.want)
		}
	}
}
```

เพิ่ม helper ท้ายไฟล์เดียวกัน:

```go
func isValidJSON(b []byte) bool {
	var v any
	return jsonUnmarshal(b, &v) == nil
}
```

- [ ] **Step 2: รัน test ให้เห็นว่า fail**

Run: `go test ./internal/model/ -run 'JSONOrRaw|ExtractRef' -v`
Expected: FAIL — ยังไม่มี `JSONOrRaw`, `ExtractRef`, `jsonUnmarshal`

- [ ] **Step 3: เขียน payload helper**

สร้าง `internal/model/payload.go`:

```go
package model

import (
	"encoding/json"
	"strconv"
)

// jsonUnmarshal แยกไว้ให้ test เรียกได้โดยไม่ต้อง import encoding/json เอง
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// JSONOrRaw ทำให้ body ใส่ลงคอลัมน์ JSONB ได้เสมอ
// body ที่ไม่ใช่ JSON (ว่าง, binary, JSON พัง) จะถูกห่อเป็น {"raw":"..."}
// ถ้าไม่ทำ INSERT จะล้มและทำให้ทั้ง request พังทั้งที่ payload แค่ผิดรูป
func JSONOrRaw(body []byte) []byte {
	if json.Valid(body) {
		return body
	}
	wrapped, err := json.Marshal(map[string]string{"raw": string(body)})
	if err != nil {
		return []byte(`{"raw":""}`)
	}
	return wrapped
}

// ExtractRef ดึงค่าอ้างอิงทางธุรกิจจาก body ตามชื่อ field ที่ group กำหนด
// หาไม่เจอหรือไม่ใช่ค่าเดี่ยว ๆ ให้คืนค่าว่าง ไม่ถือเป็น error
// เพราะ flow ที่สร้างใหม่อาจไม่มี field นี้เลยและนั่นไม่ควรทำให้ request ล้ม
func ExtractRef(body []byte, field string) string {
	if field == "" || !json.Valid(body) {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	raw, ok := m[field]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return ""
}
```

- [ ] **Step 4: รัน test ให้ผ่าน**

Run: `go test ./internal/model/ -v`
Expected: PASS ทุกเคส

- [ ] **Step 5: เขียน integration test ของ log store**

สร้าง `internal/store/logs_test.go`:

```go
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
```

- [ ] **Step 6: รัน test ให้เห็นว่า fail**

Run: `go test -tags=integration ./internal/store/ -run 'BeginRequest|RecordAttempts|FinishRequest|MarkClient|Dangerous' -v`
Expected: FAIL — ยังไม่มีเมธอดเหล่านี้

- [ ] **Step 7: เขียน implementation**

สร้าง `internal/store/logs.go`:

```go
package store

import (
	"context"
	"database/sql"
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
	for i, a := range attempts {
		n := i * 8
		placeholders = append(placeholders, fmt.Sprintf(
			"($%d,$%d,NULLIF($%d,0),$%d,NULLIF($%d,0),$%d,$%d,NULLIF($%d,''))",
			n+1, n+2, n+3, n+4, n+5, n+6, n+7, n+8))
		args = append(args, traceID, a.Seq, a.URLID, a.URL,
			a.HTTPStatus, a.DurationMS, string(a.Outcome), a.ErrMessage)
	}
	// response_body เก็บแยกเพราะเป็น TEXT ธรรมดา ไม่ต้อง validate JSON
	query := `INSERT INTO attempt_logs
	  (trace_id, seq, message_group_url_id, url, http_status, duration_ms, outcome, error_message)
	  VALUES ` + strings.Join(placeholders, ",")
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return err
	}

	for _, a := range attempts {
		if a.ResponseBody == "" {
			continue
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE attempt_logs SET response_body = $3 WHERE trace_id = $1 AND seq = $2`,
			traceID, a.Seq, a.ResponseBody); err != nil {
			return err
		}
	}
	return nil
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

var _ = sql.ErrNoRows
```

- [ ] **Step 8: รัน test ให้ผ่าน**

Run: `go test -tags=integration ./internal/store/ -v && go test ./internal/model/ -v`
Expected: PASS ทั้งหมด

- [ ] **Step 9: Commit**

```bash
git add internal/store/ internal/model/
git commit -m "feat(store): log ที่ตาม request ได้ด้วย trace_id พร้อมกติกากันเขียนทับ"
```

---

## Task 8: ชั้น AMQP — connection เดียว + RPC client pool

**Files:**
- Create: `internal/amqpx/retry.go`, `internal/amqpx/retry_test.go`
- Create: `internal/amqpx/waiters.go`, `internal/amqpx/waiters_test.go`
- Create: `internal/amqpx/conn.go`, `internal/amqpx/rpc.go`
- Create: `internal/amqpx/rpc_integration_test.go`

**Interfaces:**
- Consumes: `config.Config`
- Produces:
  - `amqpx.Retry(ctx context.Context, initial, max time.Duration, sleep func(time.Duration), fn func() error) error`
  - `amqpx.NewManager(url string) *Manager`, `(*Manager).Channel(ctx) (*amqp.Channel, error)`, `(*Manager).Close() error`
  - `amqpx.NewRPCPool(ctx context.Context, m *Manager, size int) (*RPCPool, error)`
  - `(*RPCPool).Call(ctx context.Context, queue string, pub amqp.Publishing) (*amqp.Delivery, error)`
  - `amqpx.HeaderUpstreamStatus = "x-upstream-status"` — worker ใส่ HTTP status ของ upstream มากับ reply
    เพื่อให้ฝั่ง HTTP ส่ง status code เดิมกลับ caller ได้ตาม spec §7.7
  - `(*RPCPool).Pending() int`, `(*RPCPool).Close() error`

แทนที่พฤติกรรมเดิมที่เปิด connection ใหม่ทุก request (`rabbitMQ/deposit.go:28`) ด้วย connection เดียวทั้ง process และ pool ของ channel ที่ consume `amq.rabbitmq.reply-to` ครั้งเดียวตอน start

- [ ] **Step 1: เขียน unit test ของ retry และ waiters (ไม่ต้องมี broker)**

สร้าง `internal/amqpx/retry_test.go`:

```go
package amqpx

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetrySucceedsFirstTry(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), time.Second, time.Minute,
		func(time.Duration) {}, func() error { calls++; return nil })
	if err != nil || calls != 1 {
		t.Fatalf("err=%v calls=%d, want nil/1", err, calls)
	}
}

func TestRetryBacksOffExponentiallyUpToMax(t *testing.T) {
	var slept []time.Duration
	calls := 0
	_ = Retry(context.Background(), time.Second, 4*time.Second,
		func(d time.Duration) { slept = append(slept, d) },
		func() error {
			calls++
			if calls < 5 {
				return errors.New("ยังต่อไม่ได้")
			}
			return nil
		})
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second}
	if len(slept) != len(want) {
		t.Fatalf("sleep %v, want %v", slept, want)
	}
	for i := range want {
		if slept[i] != want[i] {
			t.Errorf("sleep[%d] = %v, want %v", i, slept[i], want[i])
		}
	}
}

func TestRetryStopsWhenContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := Retry(ctx, time.Millisecond, time.Millisecond,
		func(time.Duration) { cancel() },
		func() error { calls++; return errors.New("ล้ม") })
	if err == nil {
		t.Fatal("ต้องคืน error เมื่อ context ถูกยกเลิก")
	}
	if calls > 2 {
		t.Errorf("เรียก fn %d ครั้ง — ต้องหยุดทันทีที่ ctx ถูกยกเลิก", calls)
	}
}
```

สร้าง `internal/amqpx/waiters_test.go`:

```go
package amqpx

import (
	"sync"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
)

func TestWaiterDeliversToCaller(t *testing.T) {
	w := newWaiters()
	ch := w.add("corr-1")
	if !w.deliver("corr-1", amqp.Delivery{Body: []byte("pong")}) {
		t.Fatal("deliver ต้องสำเร็จ")
	}
	if got := string((<-ch).Body); got != "pong" {
		t.Fatalf("ได้ %q, want pong", got)
	}
}

// Review Focus #1 — entry ต้องหายทุกครั้ง ไม่งั้น memory รั่วทุก request ที่ timeout
func TestWaiterRemoveLeavesNothingBehind(t *testing.T) {
	w := newWaiters()
	w.add("corr-1")
	if w.len() != 1 {
		t.Fatalf("len = %d, want 1", w.len())
	}
	w.remove("corr-1")
	if w.len() != 0 {
		t.Fatalf("len = %d, want 0 — entry ค้างคือ memory leak", w.len())
	}
}

func TestDeliverAfterRemoveDoesNotBlockOrPanic(t *testing.T) {
	w := newWaiters()
	w.add("corr-1")
	w.remove("corr-1")
	// reply มาถึงหลัง caller เลิกรอไปแล้ว — ต้องไม่ block และไม่ panic
	if w.deliver("corr-1", amqp.Delivery{Body: []byte("late")}) {
		t.Fatal("deliver ไปยัง waiter ที่ถูกลบแล้วต้องคืน false")
	}
}

func TestDeliverToUnknownCorrelationIsIgnored(t *testing.T) {
	w := newWaiters()
	if w.deliver("ไม่เคยมี", amqp.Delivery{Body: []byte("x")}) {
		t.Fatal("correlation ที่ไม่รู้จักต้องคืน false")
	}
}

func TestWaitersConcurrentUse(t *testing.T) {
	w := newWaiters()
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := string(rune('a'+n%26)) + string(rune('0'+n/26))
			ch := w.add(id)
			go w.deliver(id, amqp.Delivery{Body: []byte("ok")})
			<-ch
			w.remove(id)
		}(i)
	}
	wg.Wait()
	if w.len() != 0 {
		t.Fatalf("len = %d, want 0", w.len())
	}
}
```

- [ ] **Step 2: รัน test ให้เห็นว่า fail**

Run: `go test ./internal/amqpx/ -v`
Expected: FAIL — ยังไม่มี `Retry` และ `newWaiters`

- [ ] **Step 3: เขียน retry และ waiters**

สร้าง `internal/amqpx/retry.go`:

```go
package amqpx

import (
	"context"
	"time"
)

// Retry เรียก fn ซ้ำจนสำเร็จ โดยหน่วงแบบทวีคูณและหยุดทันทีที่ ctx ถูกยกเลิก
// แยกออกมาเป็นฟังก์ชันบริสุทธิ์เพื่อให้ทดสอบได้โดยไม่ต้องรอเวลาจริง
func Retry(ctx context.Context, initial, max time.Duration,
	sleep func(time.Duration), fn func() error) error {

	backoff := initial
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := fn()
		if err == nil {
			return nil
		}
		sleep(backoff)
		if err := ctx.Err(); err != nil {
			return err
		}
		if backoff < max {
			backoff *= 2
			if backoff > max {
				backoff = max
			}
		}
	}
}
```

สร้าง `internal/amqpx/waiters.go`:

```go
package amqpx

import (
	"sync"

	amqp "github.com/rabbitmq/amqp091-go"
)

// waiters จับคู่ correlation_id กับ goroutine ที่กำลังรอ reply อยู่
// จุดสำคัญคือทุกคนที่ add ต้อง remove เสมอแม้ตอน timeout
// ไม่งั้น map จะโตขึ้นทุก request ที่ไม่ได้รับ reply
type waiters struct {
	mu sync.Mutex
	m  map[string]chan amqp.Delivery
}

func newWaiters() *waiters { return &waiters{m: map[string]chan amqp.Delivery{}} }

// add จอง slot และคืน channel ที่มี buffer 1
// buffer สำคัญ: ทำให้ deliver ไม่ block แม้ caller จะเลิกรอไปแล้วระหว่างนั้น
func (w *waiters) add(corrID string) chan amqp.Delivery {
	ch := make(chan amqp.Delivery, 1)
	w.mu.Lock()
	w.m[corrID] = ch
	w.mu.Unlock()
	return ch
}

func (w *waiters) remove(corrID string) {
	w.mu.Lock()
	delete(w.m, corrID)
	w.mu.Unlock()
}

// deliver ส่ง reply ให้ผู้รอ คืน false ถ้าไม่มีใครรออยู่แล้ว
func (w *waiters) deliver(corrID string, d amqp.Delivery) bool {
	w.mu.Lock()
	ch, ok := w.m[corrID]
	w.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- d:
		return true
	default:
		return false
	}
}

func (w *waiters) len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.m)
}
```

- [ ] **Step 4: รัน unit test ให้ผ่าน**

Run: `go test ./internal/amqpx/ -race -v`
Expected: PASS ทุกเคส รวม `-race`

- [ ] **Step 5: เขียน Manager และ RPCPool**

สร้าง `internal/amqpx/conn.go`:

```go
package amqpx

import (
	"context"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Manager ถือ connection เดียวของทั้ง process และต่อใหม่ให้เองเมื่อหลุด
// แทนพฤติกรรมเดิมที่เปิด connection ใหม่ทุก HTTP request
type Manager struct {
	url  string
	Logf func(string, ...any)

	mu   sync.Mutex
	conn *amqp.Connection
}

func NewManager(url string) *Manager {
	return &Manager{url: url, Logf: func(string, ...any) {}}
}

// connection คืน connection ที่ยังใช้ได้ ถ้าไม่มีจะต่อใหม่พร้อม backoff
func (m *Manager) connection(ctx context.Context) (*amqp.Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.conn != nil && !m.conn.IsClosed() {
		return m.conn, nil
	}

	var conn *amqp.Connection
	err := Retry(ctx, time.Second, 30*time.Second,
		func(d time.Duration) {
			m.Logf("⏳ ต่อ RabbitMQ ไม่ได้ ลองใหม่ใน %v", d)
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		},
		func() error {
			c, err := amqp.Dial(m.url)
			if err != nil {
				return err
			}
			conn = c
			return nil
		})
	if err != nil {
		return nil, err
	}
	m.conn = conn
	return conn, nil
}

// Channel เปิด channel ใหม่บน connection ที่ใช้ร่วมกัน
// channel ถูกที่จะเปิดหลายอัน ต่างจาก connection
func (m *Manager) Channel(ctx context.Context) (*amqp.Channel, error) {
	conn, err := m.connection(ctx)
	if err != nil {
		return nil, err
	}
	return conn.Channel()
}

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn == nil || m.conn.IsClosed() {
		return nil
	}
	return m.conn.Close()
}
```

สร้าง `internal/amqpx/rpc.go`:

```go
package amqpx

import (
	"context"
	"errors"
	"sync/atomic"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	directReplyTo = "amq.rabbitmq.reply-to"

	// HeaderUpstreamStatus พา HTTP status ของ upstream กลับมาให้ฝั่ง HTTP
	// ส่งต่อ status code เดิมให้ caller ได้ ตัว body ยังเป็น passthrough ล้วน
	HeaderUpstreamStatus = "x-upstream-status"
)

type rpcChannel struct {
	ch *amqp.Channel
	w  *waiters
}

// RPCPool ถือ channel หลายช่อง แต่ละช่อง consume amq.rabbitmq.reply-to ครั้งเดียวตอน start
// ที่ต้องเป็น pool เพราะ amqp091-go ล็อกภายในตอน publish ช่องเดียวจะ serialize ทั้งระบบ
type RPCPool struct {
	chans []*rpcChannel
	next  atomic.Uint64
}

func NewRPCPool(ctx context.Context, m *Manager, size int) (*RPCPool, error) {
	if size < 1 {
		return nil, errors.New("ขนาด pool ต้องอย่างน้อย 1")
	}
	p := &RPCPool{}
	for i := 0; i < size; i++ {
		ch, err := m.Channel(ctx)
		if err != nil {
			_ = p.Close()
			return nil, err
		}
		rc := &rpcChannel{ch: ch, w: newWaiters()}

		msgs, err := ch.Consume(directReplyTo, "", true, false, false, false, nil)
		if err != nil {
			_ = p.Close()
			return nil, err
		}
		go func(rc *rpcChannel, msgs <-chan amqp.Delivery) {
			// จบเองเมื่อ channel ปิด — ห้าม block ถาวร
			for d := range msgs {
				rc.w.deliver(d.CorrelationId, d)
			}
		}(rc, msgs)

		p.chans = append(p.chans, rc)
	}
	return p, nil
}

// Call publish แล้วรอ reply ที่ correlation_id ตรงกัน
// pub.CorrelationId ต้องถูกตั้งมาจาก caller และ ReplyTo จะถูกตั้งให้เอง
func (p *RPCPool) Call(ctx context.Context, queue string, pub amqp.Publishing) (*amqp.Delivery, error) {
	if pub.CorrelationId == "" {
		return nil, errors.New("ต้องมี CorrelationId")
	}
	rc := p.chans[int(p.next.Add(1))%len(p.chans)]

	ch := rc.w.add(pub.CorrelationId)
	// remove เสมอไม่ว่าจะออกทางไหน — นี่คือสิ่งที่กัน map โตไม่รู้จบ
	defer rc.w.remove(pub.CorrelationId)

	pub.ReplyTo = directReplyTo
	if err := rc.ch.PublishWithContext(ctx, "", queue, false, false, pub); err != nil {
		return nil, err
	}

	select {
	case d := <-ch:
		return &d, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Pending บอกจำนวน caller ที่ยังรอ reply อยู่ ใช้เฝ้า leak
func (p *RPCPool) Pending() int {
	n := 0
	for _, rc := range p.chans {
		n += rc.w.len()
	}
	return n
}

func (p *RPCPool) Close() error {
	var firstErr error
	for _, rc := range p.chans {
		if err := rc.ch.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
```

- [ ] **Step 6: เขียน integration test ของ round trip จริง**

สร้าง `internal/amqpx/rpc_integration_test.go`:

```go
//go:build integration

package amqpx

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	url := os.Getenv("TEST_RABBITMQ_URL")
	if url == "" {
		t.Skip("ตั้ง TEST_RABBITMQ_URL เพื่อรัน integration test")
	}
	m := NewManager(url)
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestRPCRoundTrip(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	queue := fmt.Sprintf("mqtest_%d", time.Now().UnixNano())

	srvCh, err := m.Channel(ctx)
	if err != nil {
		t.Fatalf("เปิด channel: %v", err)
	}
	defer srvCh.Close()
	if _, err := srvCh.QueueDeclare(queue, true, false, false, false, nil); err != nil {
		t.Fatalf("declare queue: %v", err)
	}
	defer func() { _, _ = srvCh.QueueDelete(queue, false, false, false) }()

	msgs, err := srvCh.Consume(queue, "", false, false, false, false, nil)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	go func() {
		for d := range msgs {
			_ = srvCh.PublishWithContext(context.Background(), "", d.ReplyTo, false, false,
				amqp.Publishing{CorrelationId: d.CorrelationId, Body: []byte("pong:" + string(d.Body))})
			_ = d.Ack(false)
		}
	}()

	pool, err := NewRPCPool(ctx, m, 2)
	if err != nil {
		t.Fatalf("NewRPCPool: %v", err)
	}
	defer pool.Close()

	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	reply, err := pool.Call(callCtx, queue,
		amqp.Publishing{CorrelationId: "corr-1", Body: []byte("ping")})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if string(reply.Body) != "pong:ping" {
		t.Fatalf("ได้ %q, want pong:ping", reply.Body)
	}
	if pool.Pending() != 0 {
		t.Errorf("Pending = %d, want 0", pool.Pending())
	}
}

// Review Focus #1 — timeout แล้วต้องไม่เหลือ entry ค้าง
func TestRPCTimeoutLeavesNoPendingEntry(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	queue := fmt.Sprintf("mqtest_noconsumer_%d", time.Now().UnixNano())

	ch, err := m.Channel(ctx)
	if err != nil {
		t.Fatalf("เปิด channel: %v", err)
	}
	if _, err := ch.QueueDeclare(queue, true, false, false, false, nil); err != nil {
		t.Fatalf("declare queue: %v", err)
	}
	defer func() { _, _ = ch.QueueDelete(queue, false, false, false); ch.Close() }()

	pool, err := NewRPCPool(ctx, m, 1)
	if err != nil {
		t.Fatalf("NewRPCPool: %v", err)
	}
	defer pool.Close()

	for i := 0; i < 20; i++ {
		callCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		_, err := pool.Call(callCtx, queue,
			amqp.Publishing{CorrelationId: fmt.Sprintf("corr-%d", i), Body: []byte("x")})
		cancel()
		if err == nil {
			t.Fatal("ไม่มี consumer ต้อง timeout")
		}
	}
	if n := pool.Pending(); n != 0 {
		t.Fatalf("Pending = %d หลัง timeout 20 ครั้ง, want 0 — นี่คือ memory leak", n)
	}
}
```

- [ ] **Step 7: รัน test ทั้งหมดให้ผ่าน**

Run: `go test ./internal/amqpx/ -race -v && go test -tags=integration ./internal/amqpx/ -v`
Expected: PASS ทั้งสองชุด

- [ ] **Step 8: Commit**

```bash
git add internal/amqpx/
git commit -m "feat(amqpx): connection เดียวทั้ง process และ RPC pool ที่ไม่รั่ว"
```

---

## Task 9: Flow — consumer + worker pool ที่กลับมาเองได้

**Files:**
- Create: `internal/flow/flow.go`, `internal/flow/flow_test.go`

**Interfaces:**
- Consumes: `model.GroupSpec`
- Produces:
  - `flow.Broker` interface: `DeclareQueue(name string) error`, `Consume(queue string, prefetch int) (<-chan amqp.Delivery, string, error)`, `Cancel(tag string) error`, `Close() error`
  - `flow.Options{Spec model.GroupSpec, Queue string, Broker Broker, Process func(context.Context, amqp.Delivery, model.GroupSpec), Logf func(string, ...any)}`
  - `flow.New(Options) *Flow`
  - `(*Flow).Run(ctx context.Context) error` — **ต้อง return เสมอเมื่อ channel ปิด**
  - `(*Flow).Spec() model.GroupSpec`, `(*Flow).UpdateSpec(model.GroupSpec)`, `(*Flow).Drain(timeout time.Duration) error`

`Broker` เป็น interface เพื่อให้ทดสอบ lifecycle ทั้งหมดได้โดยไม่ต้องมี RabbitMQ — ตัวจริงจะถูกต่อใน Task 13

- [ ] **Step 1: เขียน test ที่ยังไม่ผ่าน**

สร้าง `internal/flow/flow_test.go`:

```go
package flow

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/celalsahinaltinisik/internal/model"
	amqp "github.com/rabbitmq/amqp091-go"
)

type fakeAck struct{ acked atomic.Int64 }

func (f *fakeAck) Ack(tag uint64, multiple bool) error              { f.acked.Add(1); return nil }
func (f *fakeAck) Nack(tag uint64, multiple, requeue bool) error    { return nil }
func (f *fakeAck) Reject(tag uint64, requeue bool) error            { return nil }

type fakeBroker struct {
	mu        sync.Mutex
	msgs      chan amqp.Delivery
	declared  []string
	prefetch  int
	cancelled bool
	failDecl  error
}

func newFakeBroker(buf int) *fakeBroker {
	return &fakeBroker{msgs: make(chan amqp.Delivery, buf)}
}

func (b *fakeBroker) DeclareQueue(name string) error {
	if b.failDecl != nil {
		return b.failDecl
	}
	b.mu.Lock()
	b.declared = append(b.declared, name)
	b.mu.Unlock()
	return nil
}

func (b *fakeBroker) Consume(queue string, prefetch int) (<-chan amqp.Delivery, string, error) {
	b.mu.Lock()
	b.prefetch = prefetch
	b.mu.Unlock()
	return b.msgs, "tag-1", nil
}

func (b *fakeBroker) Cancel(tag string) error {
	b.mu.Lock()
	if !b.cancelled {
		b.cancelled = true
		close(b.msgs) // broker จริงจะปิด channel หลัง delivery สุดท้าย
	}
	b.mu.Unlock()
	return nil
}

func (b *fakeBroker) Close() error { return nil }

func testSpec(workers int) model.GroupSpec {
	return model.GroupSpec{ID: "g1", Name: "withdraw", WorkerCount: workers,
		URLs: []model.URLSpec{{ID: 1, URL: "https://a"}}}
}

func TestRunDeclaresQueueAndSetsPrefetchToWorkerCount(t *testing.T) {
	b := newFakeBroker(0)
	f := New(Options{Spec: testSpec(7), Queue: "v2.withdraw", Broker: b,
		Process: func(context.Context, amqp.Delivery, model.GroupSpec) {}})

	done := make(chan error, 1)
	go func() { done <- f.Run(context.Background()) }()
	time.Sleep(50 * time.Millisecond)
	_ = b.Cancel("tag-1")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run ไม่ยอม return")
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.declared) != 1 || b.declared[0] != "v2.withdraw" {
		t.Errorf("declared = %v, want [v2.withdraw]", b.declared)
	}
	if b.prefetch != 7 {
		t.Errorf("prefetch = %d, want 7 — ต้องเท่ากับ worker_count ไม่งั้น worker ส่วนใหญ่จะว่าง", b.prefetch)
	}
}

// บทเรียนจาก conswithdraw.go:288 ที่ใช้ select {} แล้วกู้ตัวเองไม่ได้
func TestRunReturnsWhenDeliveryChannelCloses(t *testing.T) {
	b := newFakeBroker(0)
	f := New(Options{Spec: testSpec(3), Queue: "q", Broker: b,
		Process: func(context.Context, amqp.Delivery, model.GroupSpec) {}})

	done := make(chan error, 1)
	go func() { done <- f.Run(context.Background()) }()
	time.Sleep(20 * time.Millisecond)
	_ = b.Cancel("tag-1")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run คืน error %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run ต้อง return เมื่อ channel ปิด ห้าม block ถาวร")
	}
}

func TestRunReturnsErrorWhenDeclareFails(t *testing.T) {
	b := newFakeBroker(0)
	b.failDecl = errors.New("406 PRECONDITION_FAILED")
	f := New(Options{Spec: testSpec(1), Queue: "q", Broker: b,
		Process: func(context.Context, amqp.Delivery, model.GroupSpec) {}})

	if err := f.Run(context.Background()); err == nil {
		t.Fatal("declare ล้มต้องคืน error เพื่อให้ reconciler mark failed แล้วลองใหม่")
	}
}

func TestWorkersProcessEveryDelivery(t *testing.T) {
	b := newFakeBroker(10)
	var handled atomic.Int64
	ack := &fakeAck{}

	f := New(Options{Spec: testSpec(4), Queue: "q", Broker: b,
		Process: func(_ context.Context, d amqp.Delivery, _ model.GroupSpec) {
			handled.Add(1)
			_ = d.Ack(false)
		}})

	for i := 0; i < 10; i++ {
		b.msgs <- amqp.Delivery{Acknowledger: ack, Body: []byte("x")}
	}

	done := make(chan error, 1)
	go func() { done <- f.Run(context.Background()) }()
	time.Sleep(100 * time.Millisecond)
	_ = b.Cancel("tag-1")
	<-done

	if handled.Load() != 10 {
		t.Fatalf("ประมวลผล %d ข้อความ, want 10", handled.Load())
	}
	if ack.acked.Load() != 10 {
		t.Fatalf("ack %d ครั้ง, want 10", ack.acked.Load())
	}
}

func TestUpdateSpecIsVisibleToWorkers(t *testing.T) {
	b := newFakeBroker(2)
	seen := make(chan string, 2)
	ack := &fakeAck{}

	f := New(Options{Spec: testSpec(1), Queue: "q", Broker: b,
		Process: func(_ context.Context, d amqp.Delivery, s model.GroupSpec) {
			seen <- s.URLs[0].URL
			_ = d.Ack(false)
		}})

	done := make(chan error, 1)
	go func() { done <- f.Run(context.Background()) }()

	b.msgs <- amqp.Delivery{Acknowledger: ack}
	if got := <-seen; got != "https://a" {
		t.Fatalf("url แรก = %q", got)
	}

	next := testSpec(1)
	next.URLs = []model.URLSpec{{ID: 2, URL: "https://b"}}
	f.UpdateSpec(next)

	b.msgs <- amqp.Delivery{Acknowledger: ack}
	if got := <-seen; got != "https://b" {
		t.Fatalf("url หลัง UpdateSpec = %q, want https://b — hot-swap ต้องมีผลโดยไม่ restart", got)
	}

	_ = b.Cancel("tag-1")
	<-done
}

func TestDrainCancelsConsumerAndWaits(t *testing.T) {
	b := newFakeBroker(0)
	f := New(Options{Spec: testSpec(2), Queue: "q", Broker: b,
		Process: func(context.Context, amqp.Delivery, model.GroupSpec) {}})

	go func() { _ = f.Run(context.Background()) }()
	time.Sleep(30 * time.Millisecond)

	if err := f.Drain(2 * time.Second); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.cancelled {
		t.Fatal("Drain ต้องเรียก Cancel ก่อน ไม่งั้น consumer จะดูดงานใหม่ระหว่าง shutdown")
	}
}

func TestDrainTimesOutWhenWorkerStuck(t *testing.T) {
	b := newFakeBroker(1)
	release := make(chan struct{})
	ack := &fakeAck{}

	f := New(Options{Spec: testSpec(1), Queue: "q", Broker: b,
		Process: func(context.Context, amqp.Delivery, model.GroupSpec) { <-release }})

	go func() { _ = f.Run(context.Background()) }()
	b.msgs <- amqp.Delivery{Acknowledger: ack}
	time.Sleep(30 * time.Millisecond)

	err := f.Drain(100 * time.Millisecond)
	if err == nil {
		t.Fatal("worker ที่ค้างเกิน deadline ต้องทำให้ Drain คืน error เพื่อให้ log เห็น")
	}
	close(release)
}
```

- [ ] **Step 2: รัน test ให้เห็นว่า fail**

Run: `go test ./internal/flow/ -v`
Expected: FAIL — ยังไม่มี `New`, `Options`

- [ ] **Step 3: เขียน implementation**

สร้าง `internal/flow/flow.go`:

```go
package flow

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/celalsahinaltinisik/internal/model"
	amqp "github.com/rabbitmq/amqp091-go"
)

// Broker คือส่วนที่คุยกับ RabbitMQ แยกเป็น interface เพื่อให้ทดสอบ lifecycle ได้โดยไม่ต้องมี broker
type Broker interface {
	DeclareQueue(name string) error
	Consume(queue string, prefetch int) (<-chan amqp.Delivery, string, error)
	Cancel(tag string) error
	Close() error
}

type Options struct {
	Spec    model.GroupSpec
	Queue   string
	Broker  Broker
	Process func(ctx context.Context, d amqp.Delivery, spec model.GroupSpec)
	Logf    func(string, ...any)
}

// Flow คือหน่วยที่ reconciler สั่งการ: 1 group = 1 queue + 1 consumer + worker pool
type Flow struct {
	opts Options
	spec atomic.Pointer[model.GroupSpec]

	mu       sync.Mutex
	tag      string
	done     chan struct{}
	draining bool
}

func New(o Options) *Flow {
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	f := &Flow{opts: o, done: make(chan struct{})}
	s := o.Spec
	f.spec.Store(&s)
	return f
}

func (f *Flow) Spec() model.GroupSpec { return *f.spec.Load() }

// UpdateSpec สลับ spec แบบ atomic — worker อ่านค่าใหม่ในข้อความถัดไปโดยไม่ต้อง restart
func (f *Flow) UpdateSpec(s model.GroupSpec) { f.spec.Store(&s) }

// Run บล็อกจนกว่า channel ของ broker จะปิด แล้ว return เสมอ
//
// ห้ามใส่ select {} หรือ block ถาวรตรงนี้เด็ดขาด ระบบเก่าทำแบบนั้นที่
// conswithdraw.go:288 ทำให้ตอน AMQP หลุด worker ออกหมดแต่ goroutine ค้างถาวร
// supervisor จึงไม่เคยรู้ว่ามันตายและ readiness ยังรายงานว่าปกติ
func (f *Flow) Run(ctx context.Context) error {
	defer func() {
		f.mu.Lock()
		select {
		case <-f.done:
		default:
			close(f.done)
		}
		f.mu.Unlock()
	}()

	if err := f.opts.Broker.DeclareQueue(f.opts.Queue); err != nil {
		return err
	}

	spec := f.Spec()
	prefetch := spec.WorkerCount
	if prefetch < 1 {
		prefetch = 1
	}

	msgs, tag, err := f.opts.Broker.Consume(f.opts.Queue, prefetch)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.tag = tag
	f.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < prefetch; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range msgs {
				f.opts.Process(ctx, d, f.Spec())
			}
		}()
	}
	wg.Wait()
	return nil
}

// Drain หยุดรับงานใหม่แล้วรอให้ของในมือจบภายใน timeout
func (f *Flow) Drain(timeout time.Duration) error {
	f.mu.Lock()
	tag := f.tag
	f.draining = true
	f.mu.Unlock()

	if tag != "" {
		if err := f.opts.Broker.Cancel(tag); err != nil {
			f.opts.Logf("⚠️  cancel consumer %s ไม่สำเร็จ: %v", tag, err)
		}
	}

	select {
	case <-f.done:
		return nil
	case <-time.After(timeout):
		return errors.New("drain ไม่จบภายในเวลาที่กำหนด — ข้อความที่ยังไม่ ack จะถูก requeue")
	}
}

func (f *Flow) Draining() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.draining
}
```

- [ ] **Step 4: รัน test ให้ผ่าน**

Run: `go test ./internal/flow/ -race -v`
Expected: PASS ทุกเคส

- [ ] **Step 5: Commit**

```bash
git add internal/flow/
git commit -m "feat(flow): worker pool ที่ return เมื่อ channel ปิด และ drain ได้จริง"
```

---

## Task 10: Processor — สิ่งที่เกิดกับข้อความหนึ่งใบ

**Files:**
- Create: `internal/flow/processor.go`, `internal/flow/processor_test.go`

**Interfaces:**
- Consumes: `model`, `forward.Result`, `store.AttemptInput`, `store.FinishRequestInput`
- Produces:
  - `flow.HeaderTraceID = "x-trace-id"`, `flow.HeaderDeadline = "x-deadline"` (unix milli)
  - `flow.Sender` interface: `Send(ctx, model.GroupSpec, []byte, http.Header, time.Time) forward.Result`
  - `flow.Recorder` interface: `RecordAttempts(ctx, traceID string, []store.AttemptInput) error`, `FinishRequest(ctx, store.FinishRequestInput) error`
  - `flow.Publisher` func type: `func(ctx context.Context, replyTo string, pub amqp.Publishing) error`
  - `flow.NewProcessor(Sender, Recorder, Publisher) *Processor`
  - `(*Processor).Handle(ctx context.Context, d amqp.Delivery, spec model.GroupSpec)`

- [ ] **Step 1: เขียน test ที่ยังไม่ผ่าน**

สร้าง `internal/flow/processor_test.go`:

```go
package flow

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/celalsahinaltinisik/internal/forward"
	"github.com/celalsahinaltinisik/internal/model"
	"github.com/celalsahinaltinisik/internal/store"
	amqp "github.com/rabbitmq/amqp091-go"
)

type fakeSender struct {
	called atomic.Bool
	result forward.Result
	panics bool
}

func (f *fakeSender) Send(_ context.Context, _ model.GroupSpec, _ []byte,
	_ http.Header, _ time.Time) forward.Result {
	f.called.Store(true)
	if f.panics {
		panic("upstream client ระเบิด")
	}
	return f.result
}

type fakeRecorder struct {
	mu       sync.Mutex
	attempts []store.AttemptInput
	finished []store.FinishRequestInput
}

func (r *fakeRecorder) RecordAttempts(_ context.Context, _ string, a []store.AttemptInput) error {
	r.mu.Lock()
	r.attempts = append(r.attempts, a...)
	r.mu.Unlock()
	return nil
}

func (r *fakeRecorder) FinishRequest(_ context.Context, in store.FinishRequestInput) error {
	r.mu.Lock()
	r.finished = append(r.finished, in)
	r.mu.Unlock()
	return nil
}

func (r *fakeRecorder) lastStatus(t *testing.T) model.RequestStatus {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.finished) == 0 {
		t.Fatal("ไม่มีการเรียก FinishRequest เลย")
	}
	return r.finished[len(r.finished)-1].Status
}

func delivery(ack amqp.Acknowledger, traceID string, deadline time.Time, replyTo string) amqp.Delivery {
	h := amqp.Table{}
	if traceID != "" {
		h[HeaderTraceID] = traceID
	}
	if !deadline.IsZero() {
		h[HeaderDeadline] = strconv.FormatInt(deadline.UnixMilli(), 10)
	}
	return amqp.Delivery{
		Acknowledger:  ack,
		Headers:       h,
		CorrelationId: "corr-1",
		ReplyTo:       replyTo,
		Body:          []byte(`{"amount":100}`),
	}
}

func okResult() forward.Result {
	r := forward.Result{Attempts: []forward.Attempt{
		{Seq: 1, URLID: 1, URL: "https://a", HTTPStatus: 200,
			Outcome: model.OutcomeSuccess, Body: []byte(`{"code":0}`), Duration: 5 * time.Millisecond},
	}}
	r.Final = &r.Attempts[0]
	return r
}

func TestHandleSuccessLogsRepliesAndAcks(t *testing.T) {
	ack := &fakeAck{}
	snd := &fakeSender{result: okResult()}
	rec := &fakeRecorder{}
	var published []amqp.Publishing

	p := NewProcessor(snd, rec, func(_ context.Context, _ string, pub amqp.Publishing) error {
		published = append(published, pub)
		return nil
	})

	p.Handle(context.Background(),
		delivery(ack, "trace-1", time.Now().Add(time.Minute), "reply-q"),
		testSpec(1))

	if rec.lastStatus(t) != model.StatusSuccess {
		t.Errorf("status = %q, want success", rec.lastStatus(t))
	}
	if len(rec.attempts) != 1 {
		t.Errorf("บันทึก attempt %d แถว, want 1", len(rec.attempts))
	}
	if len(published) != 1 || string(published[0].Body) != `{"code":0}` {
		t.Errorf("reply = %+v", published)
	}
	if published[0].CorrelationId != "corr-1" {
		t.Error("reply ต้องแนบ correlation_id เดิม ไม่งั้น caller จับคู่ไม่ได้")
	}
	if ack.acked.Load() != 1 {
		t.Errorf("ack %d ครั้ง, want 1", ack.acked.Load())
	}
}

// Review Focus #2 — deadline ที่หายหรือพัง ต้องไม่ทำให้ยิง upstream แบบไร้ขอบเขต
func TestHandleTreatsMissingOrBadDeadlineAsExpired(t *testing.T) {
	for _, name := range []string{"หาย", "พัง"} {
		ack := &fakeAck{}
		snd := &fakeSender{result: okResult()}
		rec := &fakeRecorder{}

		d := delivery(ack, "trace-1", time.Time{}, "reply-q")
		if name == "พัง" {
			d.Headers[HeaderDeadline] = "ไม่ใช่ตัวเลข"
		}

		NewProcessor(snd, rec, func(context.Context, string, amqp.Publishing) error { return nil }).
			Handle(context.Background(), d, testSpec(1))

		if snd.called.Load() {
			t.Errorf("deadline %s: ห้ามยิง upstream", name)
		}
		if rec.lastStatus(t) != model.StatusExpired {
			t.Errorf("deadline %s: status = %q, want expired", name, rec.lastStatus(t))
		}
		if ack.acked.Load() != 1 {
			t.Errorf("deadline %s: ต้อง ack ทิ้ง ไม่ปล่อยให้วนซ้ำ", name)
		}
	}
}

func TestHandleSkipsUpstreamWhenDeadlinePassed(t *testing.T) {
	ack := &fakeAck{}
	snd := &fakeSender{result: okResult()}
	rec := &fakeRecorder{}
	replied := false

	NewProcessor(snd, rec, func(context.Context, string, amqp.Publishing) error {
		replied = true
		return nil
	}).Handle(context.Background(),
		delivery(ack, "trace-1", time.Now().Add(-time.Second), "reply-q"), testSpec(1))

	if snd.called.Load() {
		t.Fatal("deadline ผ่านแล้วห้ามยิง upstream — นี่คือกลไกกันออเดอร์ผี")
	}
	if replied {
		t.Error("ไม่ต้อง reply เพราะไม่มีใครรออยู่แล้ว")
	}
	if rec.lastStatus(t) != model.StatusExpired {
		t.Errorf("status = %q, want expired", rec.lastStatus(t))
	}
}

func TestHandleNoUpstreamWhenGroupHasNoURL(t *testing.T) {
	ack := &fakeAck{}
	snd := &fakeSender{result: forward.Result{}}
	rec := &fakeRecorder{}
	var published []amqp.Publishing

	spec := testSpec(1)
	spec.URLs = nil

	NewProcessor(snd, rec, func(_ context.Context, _ string, pub amqp.Publishing) error {
		published = append(published, pub)
		return nil
	}).Handle(context.Background(),
		delivery(ack, "trace-1", time.Now().Add(time.Minute), "reply-q"), spec)

	if rec.lastStatus(t) != model.StatusNoUpstream {
		t.Errorf("status = %q, want no_upstream", rec.lastStatus(t))
	}
	if len(published) != 1 {
		t.Fatal("ต้อง reply บอก caller ว่าไม่มีปลายทาง ไม่ใช่ปล่อยให้รอจน timeout")
	}
	if ack.acked.Load() != 1 {
		t.Error("ต้อง ack")
	}
}

func TestHandleFatalOutcomeStillRepliesAndAcks(t *testing.T) {
	ack := &fakeAck{}
	res := forward.Result{Attempts: []forward.Attempt{
		{Seq: 1, URL: "https://a", HTTPStatus: 400, Outcome: model.OutcomeFatal,
			Body: []byte(`{"message":"ยอดเงินไม่พอ"}`)},
	}}
	res.Final = &res.Attempts[0]
	rec := &fakeRecorder{}
	var published []amqp.Publishing

	NewProcessor(&fakeSender{result: res}, rec,
		func(_ context.Context, _ string, pub amqp.Publishing) error {
			published = append(published, pub)
			return nil
		}).Handle(context.Background(),
		delivery(ack, "trace-1", time.Now().Add(time.Minute), "reply-q"), testSpec(1))

	if rec.lastStatus(t) != model.StatusFailed {
		t.Errorf("status = %q, want failed", rec.lastStatus(t))
	}
	if len(published) != 1 || string(published[0].Body) != `{"message":"ยอดเงินไม่พอ"}` {
		t.Error("ต้องส่ง response ของ upstream กลับไปตรง ๆ ให้ caller เห็นเหตุผล")
	}
	if ack.acked.Load() != 1 {
		t.Error("ล้มเหลวก็ต้อง ack — ไม่ requeue เพราะ caller รอแบบ synchronous")
	}
}

func TestHandleRecoversFromPanicAndAcks(t *testing.T) {
	ack := &fakeAck{}
	rec := &fakeRecorder{}

	NewProcessor(&fakeSender{panics: true}, rec,
		func(context.Context, string, amqp.Publishing) error { return nil }).
		Handle(context.Background(),
			delivery(ack, "trace-1", time.Now().Add(time.Minute), "reply-q"), testSpec(1))

	if ack.acked.Load() != 1 {
		t.Fatal("panic แล้วต้อง ack ไม่งั้น poison message จะวนไม่รู้จบ")
	}
	if rec.lastStatus(t) != model.StatusFailed {
		t.Errorf("status = %q, want failed", rec.lastStatus(t))
	}
}

func TestHandleWithoutReplyToSkipsPublishButStillFinishes(t *testing.T) {
	ack := &fakeAck{}
	rec := &fakeRecorder{}
	replied := false

	NewProcessor(&fakeSender{result: okResult()}, rec,
		func(context.Context, string, amqp.Publishing) error { replied = true; return nil }).
		Handle(context.Background(),
			delivery(ack, "trace-1", time.Now().Add(time.Minute), ""), testSpec(1))

	if replied {
		t.Error("ไม่มี ReplyTo ต้องไม่ publish")
	}
	if rec.lastStatus(t) != model.StatusSuccess {
		t.Error("ยังต้องบันทึกผลตามปกติ")
	}
	if ack.acked.Load() != 1 {
		t.Error("ต้อง ack")
	}
}
```

เพิ่ม import `"sync/atomic"` ในไฟล์ test ด้วย (ใช้ใน `fakeSender.called`)

- [ ] **Step 2: รัน test ให้เห็นว่า fail**

Run: `go test ./internal/flow/ -run Handle -v`
Expected: FAIL — ยังไม่มี `NewProcessor`

- [ ] **Step 3: เขียน implementation**

สร้าง `internal/flow/processor.go`:

```go
package flow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/celalsahinaltinisik/internal/amqpx"
	"github.com/celalsahinaltinisik/internal/forward"
	"github.com/celalsahinaltinisik/internal/model"
	"github.com/celalsahinaltinisik/internal/store"
	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	HeaderTraceID  = "x-trace-id"
	HeaderDeadline = "x-deadline" // unix milli
)

type Sender interface {
	Send(ctx context.Context, spec model.GroupSpec, body []byte,
		hdr http.Header, deadline time.Time) forward.Result
}

type Recorder interface {
	RecordAttempts(ctx context.Context, traceID string, attempts []store.AttemptInput) error
	FinishRequest(ctx context.Context, in store.FinishRequestInput) error
}

type Publisher func(ctx context.Context, replyTo string, pub amqp.Publishing) error

type Processor struct {
	sender  Sender
	rec     Recorder
	publish Publisher
	Now     func() time.Time
	Logf    func(string, ...any)
}

func NewProcessor(s Sender, r Recorder, p Publisher) *Processor {
	return &Processor{sender: s, rec: r, publish: p,
		Now: time.Now, Logf: func(string, ...any) {}}
}

// Handle จัดการข้อความหนึ่งใบจนจบ แล้ว ack เสมอ
//
// ไม่ requeue ในทุกกรณี เพราะ caller รอแบบ synchronous การ requeue
// จะสร้างออเดอร์ซ้ำให้คนที่เดินจากไปแล้ว ความล้มเหลวถูกบันทึกใน log แทน
func (p *Processor) Handle(ctx context.Context, d amqp.Delivery, spec model.GroupSpec) {
	start := p.Now()
	traceID := headerString(d.Headers, HeaderTraceID)

	defer func() {
		if r := recover(); r != nil {
			p.Logf("💥 panic ระหว่างประมวลผล trace=%s: %v", traceID, r)
			p.finish(ctx, traceID, model.StatusFailed, nil, 0, start, fmt.Sprintf("panic: %v", r))
			p.reply(ctx, d, errorBody(500, "internal error"), 500)
		}
		_ = d.Ack(false)
	}()

	deadline, ok := parseDeadline(d.Headers)
	if !ok || !p.Now().Before(deadline) {
		// ข้อความหมดอายุ — caller เลิกรอไปแล้ว ห้ามยิง upstream
		// นี่คือกลไกที่กันออเดอร์ผีแบบที่ระบบเก่ามีตอน consumer กลับมาหลัง deploy
		p.finish(ctx, traceID, model.StatusExpired, nil, 0, start, "ข้อความหมดอายุก่อนถูกประมวลผล")
		return
	}

	if !spec.HasUpstream() {
		p.finish(ctx, traceID, model.StatusNoUpstream, nil, 0, start, "group ไม่มี url ที่ active")
		p.reply(ctx, d, errorBody(503, "ไม่มีปลายทางที่พร้อมใช้งาน"), 503)
		return
	}

	res := p.sender.Send(ctx, spec, d.Body, amqpHeadersToHTTP(d.Headers), deadline)

	if len(res.Attempts) > 0 {
		if err := p.rec.RecordAttempts(ctx, traceID, toAttemptInputs(res.Attempts)); err != nil {
			p.Logf("⚠️  บันทึก attempt_logs ไม่สำเร็จ trace=%s: %v", traceID, err)
		}
	}

	if res.Final == nil {
		p.finish(ctx, traceID, model.StatusFailed, nil, len(res.Attempts), start, "ไม่ได้ยิง upstream เลย")
		p.reply(ctx, d, errorBody(504, "หมดเวลาก่อนได้ยิงปลายทาง"), 504)
		return
	}

	status := model.StatusFailed
	if res.Final.Outcome == model.OutcomeSuccess {
		status = model.StatusSuccess
	}
	p.finish(ctx, traceID, status, res.Final.Body, len(res.Attempts), start, res.Final.ErrMessage)

	body := res.Final.Body
	if len(body) == 0 {
		body = errorBody(res.Final.HTTPStatus, res.Final.ErrMessage)
	}
	upstream := res.Final.HTTPStatus
	if upstream == 0 {
		upstream = 502 // ยิงไม่ถึงปลายทางเลย
	}
	p.reply(ctx, d, body, upstream)
}

func (p *Processor) finish(ctx context.Context, traceID string, st model.RequestStatus,
	body []byte, attempts int, start time.Time, errMsg string) {
	if traceID == "" {
		return
	}
	err := p.rec.FinishRequest(ctx, store.FinishRequestInput{
		TraceID: traceID, Status: st, ResponseBody: body,
		AttemptCount: attempts,
		TotalMS:      int(p.Now().Sub(start) / time.Millisecond),
		ErrMessage:   errMsg,
	})
	if err != nil {
		p.Logf("⚠️  อัปเดต request_logs ไม่สำเร็จ trace=%s: %v", traceID, err)
	}
}

func (p *Processor) reply(ctx context.Context, d amqp.Delivery, body []byte, upstreamStatus int) {
	if d.ReplyTo == "" {
		return
	}
	err := p.publish(ctx, d.ReplyTo, amqp.Publishing{
		ContentType:   "application/json",
		CorrelationId: d.CorrelationId,
		Headers:       amqp.Table{amqpx.HeaderUpstreamStatus: int32(upstreamStatus)},
		Body:          body,
	})
	if err != nil {
		p.Logf("⚠️  ส่ง reply ไม่สำเร็จ corr=%s: %v", d.CorrelationId, err)
	}
}

func toAttemptInputs(as []forward.Attempt) []store.AttemptInput {
	out := make([]store.AttemptInput, 0, len(as))
	for _, a := range as {
		out = append(out, store.AttemptInput{
			Seq: a.Seq, URLID: a.URLID, URL: a.URL,
			HTTPStatus: a.HTTPStatus,
			DurationMS: int(a.Duration / time.Millisecond),
			Outcome:    a.Outcome,
			ResponseBody: string(a.Body),
			ErrMessage: a.ErrMessage,
		})
	}
	return out
}

func parseDeadline(h amqp.Table) (time.Time, bool) {
	raw := headerString(h, HeaderDeadline)
	if raw == "" {
		return time.Time{}, false
	}
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.UnixMilli(ms), true
}

func headerString(h amqp.Table, key string) string {
	if h == nil {
		return ""
	}
	switch v := h[key].(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return ""
	}
}

// amqpHeadersToHTTP แปลง header ของ caller ที่ติดมากับข้อความกลับเป็น http.Header
// header ภายในของเราเอง (x-trace-id, x-deadline) ไม่ถูกส่งต่อไป upstream
func amqpHeadersToHTTP(h amqp.Table) http.Header {
	out := http.Header{}
	for k, v := range h {
		if k == HeaderTraceID || k == HeaderDeadline {
			continue
		}
		if s, ok := v.(string); ok {
			out.Add(k, s)
		}
	}
	return out
}

func errorBody(code int, msg string) []byte {
	b, err := json.Marshal(map[string]any{"code": code, "message": msg})
	if err != nil {
		return []byte(`{"code":500,"message":"internal error"}`)
	}
	return b
}
```

- [ ] **Step 4: รัน test ให้ผ่าน**

Run: `go test ./internal/flow/ -race -v`
Expected: PASS ทุกเคส รวม Task 9 ด้วย

- [ ] **Step 5: Commit**

```bash
git add internal/flow/
git commit -m "feat(flow): processor ที่เคารพ deadline และ ack เสมอ"
```

---

## Task 11: Registry และ Reconciler

**Files:**
- Create: `internal/flow/registry.go`, `internal/flow/registry_test.go`
- Create: `internal/reconcile/diff.go`, `internal/reconcile/diff_test.go`
- Create: `internal/reconcile/loop.go`

**Interfaces:**
- Consumes: `flow.Flow`, `model.GroupSpec`
- Produces:
  - `flow.State` (`StateStarting|StateRunning|StateDegraded|StateDraining|StateFailed`)
  - `flow.Entry{ID, Name, Revision string, WorkerCount int, State State}`
  - `flow.NewRegistry() *Registry` พร้อม `Put`, `SetState`, `Remove`, `ByName`, `Snapshot`, `AllRunning`
  - `reconcile.ActionKind` (`Start|HotSwap|Restart|Stop|Skip`)
  - `reconcile.Action{Kind ActionKind, Spec model.GroupSpec, ID string, Reason string}`
  - `reconcile.Diff(desired []model.GroupSpec, actual map[string]flow.Entry) []Action` — ฟังก์ชันบริสุทธิ์
  - `reconcile.Loop` + `(*Loop).Once(ctx) error` + `(*Loop).Run(ctx)`

- [ ] **Step 1: เขียน test ของ registry**

สร้าง `internal/flow/registry_test.go`:

```go
package flow

import "testing"

func TestRegistryPutAndLookupByName(t *testing.T) {
	r := NewRegistry()
	f := New(Options{Spec: testSpec(1), Queue: "q", Broker: newFakeBroker(0),
		Process: func(context.Context, amqp.Delivery, model.GroupSpec) {}})

	r.Put("g1", f, StateRunning)

	got, st, ok := r.ByName("withdraw")
	if !ok || got != f || st != StateRunning {
		t.Fatalf("ByName = %v %v %v", got, st, ok)
	}
	if _, _, ok := r.ByName("ไม่มี"); ok {
		t.Error("ชื่อที่ไม่มีต้องคืน false")
	}
}

func TestRegistrySnapshotCarriesRevision(t *testing.T) {
	r := NewRegistry()
	spec := testSpec(3)
	f := New(Options{Spec: spec, Queue: "q", Broker: newFakeBroker(0),
		Process: func(context.Context, amqp.Delivery, model.GroupSpec) {}})
	r.Put("g1", f, StateRunning)

	snap := r.Snapshot()
	e, ok := snap["g1"]
	if !ok {
		t.Fatal("ไม่พบ g1 ใน snapshot")
	}
	if e.Revision != spec.Revision() {
		t.Error("Revision ใน snapshot ต้องมาจาก spec ปัจจุบันของ flow")
	}
	if e.WorkerCount != 3 || e.Name != "withdraw" {
		t.Errorf("entry = %+v", e)
	}
}

func TestRegistryAllRunningReportsPerFlow(t *testing.T) {
	r := NewRegistry()
	mk := func(name string) *Flow {
		s := testSpec(1)
		s.Name = name
		return New(Options{Spec: s, Queue: "q", Broker: newFakeBroker(0),
			Process: func(context.Context, amqp.Delivery, model.GroupSpec) {}})
	}
	r.Put("g1", mk("withdraw"), StateRunning)
	r.Put("g2", mk("deposit"), StateFailed)

	ok, states := r.AllRunning()
	if ok {
		t.Error("มี flow ที่ failed อยู่ ต้องไม่ ready")
	}
	if states["deposit"] != StateFailed {
		t.Errorf("states = %v", states)
	}
}

func TestRegistryRemove(t *testing.T) {
	r := NewRegistry()
	f := New(Options{Spec: testSpec(1), Queue: "q", Broker: newFakeBroker(0),
		Process: func(context.Context, amqp.Delivery, model.GroupSpec) {}})
	r.Put("g1", f, StateRunning)
	r.Remove("g1")
	if _, _, ok := r.ByName("withdraw"); ok {
		t.Fatal("ลบแล้วต้องหาไม่เจอ")
	}
	if len(r.Snapshot()) != 0 {
		t.Fatal("snapshot ต้องว่าง")
	}
}
```

เพิ่ม import `"context"` และ `amqp "github.com/rabbitmq/amqp091-go"` และ `"github.com/celalsahinaltinisik/internal/model"` ในไฟล์นี้

- [ ] **Step 2: เขียน registry**

สร้าง `internal/flow/registry.go`:

```go
package flow

import "sync"

type State string

const (
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateDegraded State = "degraded" // ไม่มี url ที่ active
	StateDraining State = "draining"
	StateFailed   State = "failed"
)

// Entry คือภาพนิ่งของ flow หนึ่งตัว ใช้ให้ reconciler เทียบกับ desired state
type Entry struct {
	ID          string
	Name        string
	Revision    string
	WorkerCount int
	State       State
}

type Registry struct {
	mu    sync.RWMutex
	flows map[string]*Flow
	state map[string]State
}

func NewRegistry() *Registry {
	return &Registry{flows: map[string]*Flow{}, state: map[string]State{}}
}

func (r *Registry) Put(id string, f *Flow, st State) {
	r.mu.Lock()
	r.flows[id] = f
	r.state[id] = st
	r.mu.Unlock()
}

func (r *Registry) SetState(id string, st State) {
	r.mu.Lock()
	if _, ok := r.flows[id]; ok {
		r.state[id] = st
	}
	r.mu.Unlock()
}

func (r *Registry) Remove(id string) {
	r.mu.Lock()
	delete(r.flows, id)
	delete(r.state, id)
	r.mu.Unlock()
}

func (r *Registry) Get(id string) (*Flow, State, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.flows[id]
	return f, r.state[id], ok
}

// ByName ใช้จากฝั่ง HTTP เพื่อหา flow จาก path
func (r *Registry) ByName(name string) (*Flow, State, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for id, f := range r.flows {
		if f.Spec().Name == name {
			return f, r.state[id], true
		}
	}
	return nil, "", false
}

func (r *Registry) Snapshot() map[string]Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]Entry, len(r.flows))
	for id, f := range r.flows {
		s := f.Spec()
		out[id] = Entry{ID: id, Name: s.Name, Revision: s.Revision(),
			WorkerCount: s.WorkerCount, State: r.state[id]}
	}
	return out
}

// AllRunning ใช้ตอบ /readyz — degraded ถือว่ายัง ready เพราะ service ทำงานถูกต้องแล้ว
// แค่ไม่มีปลายทางให้ยิง ซึ่งเป็นเรื่องของ config ไม่ใช่ความพร้อมของ process
func (r *Registry) AllRunning() (bool, map[string]State) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := map[string]State{}
	ok := true
	for id, f := range r.flows {
		st := r.state[id]
		out[f.Spec().Name] = st
		if st != StateRunning && st != StateDegraded {
			ok = false
		}
	}
	return ok, out
}
```

- [ ] **Step 3: เขียน test ของ Diff**

สร้าง `internal/reconcile/diff_test.go`:

```go
package reconcile

import (
	"testing"

	"github.com/celalsahinaltinisik/internal/flow"
	"github.com/celalsahinaltinisik/internal/model"
)

func spec(id, name string, workers int, urls ...string) model.GroupSpec {
	g := model.GroupSpec{ID: id, Name: name, WorkerCount: workers}
	for i, u := range urls {
		g.URLs = append(g.URLs, model.URLSpec{ID: int64(i + 1), URL: u})
	}
	return g
}

func entryOf(s model.GroupSpec, st flow.State) flow.Entry {
	return flow.Entry{ID: s.ID, Name: s.Name, Revision: s.Revision(),
		WorkerCount: s.WorkerCount, State: st}
}

func only(t *testing.T, actions []Action) Action {
	t.Helper()
	if len(actions) != 1 {
		t.Fatalf("actions = %d (%+v), want 1", len(actions), actions)
	}
	return actions[0]
}

func TestNewGroupProducesStart(t *testing.T) {
	s := spec("g1", "withdraw", 50, "https://a")
	a := only(t, Diff([]model.GroupSpec{s}, map[string]flow.Entry{}))
	if a.Kind != Start || a.Spec.Name != "withdraw" {
		t.Fatalf("action = %+v, want Start withdraw", a)
	}
}

func TestUnchangedGroupProducesNothing(t *testing.T) {
	s := spec("g1", "withdraw", 50, "https://a")
	got := Diff([]model.GroupSpec{s}, map[string]flow.Entry{"g1": entryOf(s, flow.StateRunning)})
	if len(got) != 0 {
		t.Fatalf("actions = %+v, want ว่าง", got)
	}
}

func TestURLChangeProducesHotSwap(t *testing.T) {
	old := spec("g1", "withdraw", 50, "https://a")
	next := spec("g1", "withdraw", 50, "https://a", "https://b")
	a := only(t, Diff([]model.GroupSpec{next}, map[string]flow.Entry{"g1": entryOf(old, flow.StateRunning)}))
	if a.Kind != HotSwap {
		t.Fatalf("action = %+v, want HotSwap — เปลี่ยน url ไม่ควรต้อง restart consumer", a)
	}
}

func TestWorkerCountChangeProducesRestart(t *testing.T) {
	old := spec("g1", "withdraw", 50, "https://a")
	next := spec("g1", "withdraw", 80, "https://a")
	a := only(t, Diff([]model.GroupSpec{next}, map[string]flow.Entry{"g1": entryOf(old, flow.StateRunning)}))
	if a.Kind != Restart {
		t.Fatalf("action = %+v, want Restart — prefetch ผูกกับ channel จึงต้องเปิดใหม่", a)
	}
}

func TestRenameProducesRestartNotStopAndStart(t *testing.T) {
	old := spec("g1", "withdraw", 50, "https://a")
	next := spec("g1", "withdraw-v2", 50, "https://a")
	a := only(t, Diff([]model.GroupSpec{next}, map[string]flow.Entry{"g1": entryOf(old, flow.StateRunning)}))
	if a.Kind != Restart {
		t.Fatalf("action = %+v, want Restart — UUID เดิมแปลว่าย้ายบ้าน ไม่ใช่ลบแล้วสร้างใหม่", a)
	}
}

func TestMissingFromDesiredProducesStop(t *testing.T) {
	old := spec("g1", "withdraw", 50, "https://a")
	a := only(t, Diff(nil, map[string]flow.Entry{"g1": entryOf(old, flow.StateRunning)}))
	if a.Kind != Stop || a.ID != "g1" {
		t.Fatalf("action = %+v, want Stop g1", a)
	}
}

func TestFailedFlowIsRestarted(t *testing.T) {
	s := spec("g1", "withdraw", 50, "https://a")
	a := only(t, Diff([]model.GroupSpec{s}, map[string]flow.Entry{"g1": entryOf(s, flow.StateFailed)}))
	if a.Kind != Restart {
		t.Fatalf("action = %+v, want Restart — flow ที่ failed ต้องถูกกู้", a)
	}
}

// Review Focus #5 — ชื่อผิดกติกาต้องไม่ทำให้ flow อื่นไม่ได้ขึ้น
func TestInvalidGroupNameIsSkippedWithoutBlockingOthers(t *testing.T) {
	bad := spec("g1", "Withdraw Prod", 50, "https://a")
	good := spec("g2", "deposit", 50, "https://b")

	actions := Diff([]model.GroupSpec{bad, good}, map[string]flow.Entry{})
	if len(actions) != 2 {
		t.Fatalf("actions = %d (%+v), want 2", len(actions), actions)
	}
	var sawSkip, sawStart bool
	for _, a := range actions {
		switch a.Kind {
		case Skip:
			sawSkip = true
			if a.Reason == "" {
				t.Error("Skip ต้องมีเหตุผลให้ log")
			}
		case Start:
			sawStart = true
			if a.Spec.Name != "deposit" {
				t.Errorf("ตัวที่ Start ควรเป็น deposit แต่เป็น %s", a.Spec.Name)
			}
		}
	}
	if !sawSkip || !sawStart {
		t.Fatalf("ต้องมีทั้ง Skip และ Start แต่ได้ %+v", actions)
	}
}

func TestReservedNameIsSkipped(t *testing.T) {
	a := only(t, Diff([]model.GroupSpec{spec("g1", "healthz", 50, "https://a")}, map[string]flow.Entry{}))
	if a.Kind != Skip {
		t.Fatalf("action = %+v, want Skip", a)
	}
}
```

- [ ] **Step 4: รัน test ให้เห็นว่า fail**

Run: `go test ./internal/flow/ ./internal/reconcile/ -v`
Expected: FAIL — ยังไม่มี `Diff`

- [ ] **Step 5: เขียน Diff**

สร้าง `internal/reconcile/diff.go`:

```go
package reconcile

import (
	"github.com/celalsahinaltinisik/internal/flow"
	"github.com/celalsahinaltinisik/internal/model"
)

type ActionKind string

const (
	Start   ActionKind = "start"
	HotSwap ActionKind = "hotswap"
	Restart ActionKind = "restart"
	Stop    ActionKind = "stop"
	Skip    ActionKind = "skip"
)

type Action struct {
	Kind   ActionKind
	ID     string
	Spec   model.GroupSpec
	Reason string
}

// Diff เทียบสิ่งที่ DB บอกว่าควรเป็น กับสิ่งที่รันอยู่จริง แล้วบอกว่าต้องทำอะไร
// เป็นฟังก์ชันบริสุทธิ์ทั้งหมดเพื่อให้ทดสอบทุกเคสได้โดยไม่ต้องมี broker หรือ DB
func Diff(desired []model.GroupSpec, actual map[string]flow.Entry) []Action {
	var actions []Action
	seen := map[string]bool{}

	for _, want := range desired {
		if err := want.ValidateName(); err != nil {
			// ข้ามตัวที่ชื่อใช้ไม่ได้ แต่ตัวอื่นต้องทำงานต่อได้ตามปกติ
			actions = append(actions, Action{Kind: Skip, ID: want.ID, Spec: want, Reason: err.Error()})
			continue
		}
		seen[want.ID] = true

		have, running := actual[want.ID]
		switch {
		case !running:
			actions = append(actions, Action{Kind: Start, ID: want.ID, Spec: want})
		case have.State == flow.StateFailed:
			actions = append(actions, Action{Kind: Restart, ID: want.ID, Spec: want,
				Reason: "flow อยู่ในสถานะ failed"})
		case have.Revision == want.Revision():
			// ไม่มีอะไรเปลี่ยน
		case have.WorkerCount != want.WorkerCount:
			actions = append(actions, Action{Kind: Restart, ID: want.ID, Spec: want,
				Reason: "worker_count เปลี่ยน ต้องตั้ง prefetch ใหม่บน channel"})
		case have.Name != want.Name:
			actions = append(actions, Action{Kind: Restart, ID: want.ID, Spec: want,
				Reason: "group_name เปลี่ยน ต้องย้ายไป queue ใหม่"})
		default:
			actions = append(actions, Action{Kind: HotSwap, ID: want.ID, Spec: want})
		}
	}

	for id, have := range actual {
		if !seen[id] {
			actions = append(actions, Action{Kind: Stop, ID: id,
				Reason: "ไม่มีใน DB แล้ว", Spec: model.GroupSpec{Name: have.Name}})
		}
	}
	return actions
}
```

- [ ] **Step 6: เขียน loop ที่เอา action ไปใช้**

สร้าง `internal/reconcile/loop.go`:

```go
package reconcile

import (
	"context"
	"time"

	"github.com/celalsahinaltinisik/internal/flow"
	"github.com/celalsahinaltinisik/internal/model"
)

type GroupLoader interface {
	LoadGroups(ctx context.Context) ([]model.GroupSpec, error)
}

// FlowFactory สร้าง Flow ตัวใหม่จาก spec — cmd/gateway เป็นคนใส่ของจริงเข้ามา
type FlowFactory func(spec model.GroupSpec) (*flow.Flow, error)

type Loop struct {
	Loader   GroupLoader
	Registry *flow.Registry
	Factory  FlowFactory
	Interval time.Duration
	Drain    time.Duration
	Logf     func(string, ...any)
}

// Once ทำ reconcile หนึ่งรอบ ใช้ทั้งตอน start (แบบ sync) และในลูป
func (l *Loop) Once(ctx context.Context) error {
	desired, err := l.Loader.LoadGroups(ctx)
	if err != nil {
		return err
	}
	for _, a := range Diff(desired, l.Registry.Snapshot()) {
		l.apply(ctx, a)
	}
	return nil
}

func (l *Loop) apply(ctx context.Context, a Action) {
	switch a.Kind {
	case Skip:
		l.Logf("⏭  ข้าม group %s: %s", a.Spec.Name, a.Reason)

	case Start:
		l.start(ctx, a.Spec)

	case HotSwap:
		if f, _, ok := l.Registry.Get(a.ID); ok {
			f.UpdateSpec(a.Spec)
			l.Registry.SetState(a.ID, stateFor(a.Spec))
			l.Logf("🔄 %s: อัปเดต url/config โดยไม่ restart", a.Spec.Name)
		}

	case Restart:
		l.Logf("♻️  %s: restart (%s)", a.Spec.Name, a.Reason)
		l.stop(a.ID)
		l.start(ctx, a.Spec)

	case Stop:
		l.Logf("🛑 %s: ปิด (%s)", a.Spec.Name, a.Reason)
		l.stop(a.ID)
	}
}

func (l *Loop) start(ctx context.Context, spec model.GroupSpec) {
	f, err := l.Factory(spec)
	if err != nil {
		l.Logf("❌ %s: สร้าง flow ไม่สำเร็จ: %v", spec.Name, err)
		return
	}
	// ต้อง Put ด้วยสถานะสุดท้ายก่อนสตาร์ท goroutine
	// ถ้า Put เป็น starting แล้วค่อย SetState ทีหลัง goroutine ที่ Run ล้มทันที
	// (เช่น declare queue เจอ 406) จะ set failed ก่อน แล้วโดนเขียนทับเป็น running
	// ทำให้ reconciler ไม่รู้ว่า flow ตายและไม่กู้ให้
	l.Registry.Put(spec.ID, f, stateFor(spec))

	go func() {
		// Run คืนค่าเมื่อ channel ปิด — ถือเป็นการตายที่ต้องกู้ในรอบถัดไป
		if err := f.Run(ctx); err != nil {
			l.Logf("❌ %s: flow หยุดพร้อม error: %v", spec.Name, err)
		} else if !f.Draining() {
			l.Logf("⏹  %s: flow หยุดเอง จะกู้ในรอบ reconcile ถัดไป", spec.Name)
		}
		if !f.Draining() {
			l.Registry.SetState(spec.ID, flow.StateFailed)
		}
	}()

	l.Logf("▶️  %s: ทำงานแล้ว (worker=%d, url=%d)", spec.Name, spec.WorkerCount, len(spec.URLs))
}

func (l *Loop) stop(id string) {
	f, _, ok := l.Registry.Get(id)
	if !ok {
		return
	}
	l.Registry.SetState(id, flow.StateDraining)
	if err := f.Drain(l.Drain); err != nil {
		l.Logf("⚠️  drain ไม่จบในเวลา: %v", err)
	}
	l.Registry.Remove(id)
}

func stateFor(s model.GroupSpec) flow.State {
	if !s.HasUpstream() {
		return flow.StateDegraded
	}
	return flow.StateRunning
}

// Run วน reconcile จนกว่า ctx จะถูกยกเลิก
func (l *Loop) Run(ctx context.Context) {
	t := time.NewTicker(l.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := l.Once(ctx); err != nil {
				l.Logf("⚠️  reconcile ล้มเหลว: %v", err)
			}
		}
	}
}
```

- [ ] **Step 7: รัน test ให้ผ่าน**

Run: `go test ./internal/... -race`
Expected: PASS ทุกแพ็กเกจ

- [ ] **Step 8: Commit**

```bash
git add internal/flow/ internal/reconcile/
git commit -m "feat(reconcile): เทียบ desired กับ actual แล้วปรับ flow ให้ตรงกัน"
```

---

## Task 12: HTTP API — route แบบ dynamic, whitelist, healthz/readyz

**Files:**
- Create: `internal/httpapi/clientip.go`, `internal/httpapi/clientip_test.go`
- Create: `internal/httpapi/handler.go`, `internal/httpapi/handler_test.go`

**Interfaces:**
- Consumes: `flow.Registry`, `config.Config`, `store.BeginRequestInput`, `amqpx.HeaderUpstreamStatus`
- Produces:
  - `httpapi.ClientIP(r *http.Request, trustedProxies int) string`
  - `httpapi.IsAllowed(ip string, allowed []string, allowAll bool) bool`
  - `httpapi.RequestLogger` interface: `BeginRequest(ctx, store.BeginRequestInput) error`, `MarkClientOutcome(ctx, traceID string, httpStatus int) error`
  - `httpapi.RPCCaller` interface: `Call(ctx, queue string, pub amqp.Publishing) (*amqp.Delivery, error)`
  - `httpapi.New(Options) *Handler` ที่ implement `http.Handler`

> ⚠️ **การเปลี่ยนพฤติกรรมที่ต้องยืนยันกับ caller ก่อน cutover**
> ระบบเก่าตอบ **200 เสมอ** พร้อม body ของ upstream (`controllers/homeController.go:251-252`) caller จึงน่าจะอ่านฟิลด์ `code` ใน JSON ไม่ใช่ HTTP status
> spec §7.7 สั่งให้ส่ง status code เดิมของ upstream กลับไป ซึ่งแปลว่า caller ที่เคยได้ 200 ตลอดจะเริ่มได้ 400/500
> แผนนี้ทำตาม spec แต่**ต้องทดสอบกับ caller จริงในระยะที่ 2 ของ cutover (spec §8.5) ก่อนเปลี่ยน route**

- [ ] **Step 1: เขียน test ของ client IP (จุดที่ระบบเก่าโดนปลอมได้)**

สร้าง `internal/httpapi/clientip_test.go`:

```go
package httpapi

import (
	"net/http"
	"testing"
)

func reqWith(remote, xff string) *http.Request {
	r := &http.Request{RemoteAddr: remote, Header: http.Header{}}
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return r
}

func TestClientIPIgnoresXFFWhenNoTrustedProxy(t *testing.T) {
	got := ClientIP(reqWith("203.0.113.9:5555", "1.2.3.4"), 0)
	if got != "203.0.113.9" {
		t.Fatalf("ClientIP = %q, want 203.0.113.9 — ไม่มี proxy ต้องไม่เชื่อ XFF เลย", got)
	}
}

// นี่คือบั๊กของระบบเก่า: homeController.go:75 เอาตัวซ้ายสุดซึ่ง caller ปลอมได้
func TestClientIPCountsFromRightSoSpoofingFails(t *testing.T) {
	// attacker ยิงผ่าน proxy พร้อมแนบ XFF ปลอม proxy จะ append IP จริงต่อท้าย
	got := ClientIP(reqWith("10.0.0.5:443", "9.9.9.9, 203.0.113.9"), 1)
	if got != "203.0.113.9" {
		t.Fatalf("ClientIP = %q, want 203.0.113.9 — ค่าที่ attacker ใส่เองต้องถูกข้าม", got)
	}
}

func TestClientIPWithTwoProxies(t *testing.T) {
	got := ClientIP(reqWith("10.0.0.5:443", "9.9.9.9, 203.0.113.9, 10.0.0.9"), 2)
	if got != "203.0.113.9" {
		t.Fatalf("ClientIP = %q, want 203.0.113.9", got)
	}
}

func TestClientIPFallsBackWhenChainShorterThanExpected(t *testing.T) {
	got := ClientIP(reqWith("10.0.0.5:443", ""), 2)
	if got != "10.0.0.5" {
		t.Fatalf("ClientIP = %q, want 10.0.0.5", got)
	}
}

func TestClientIPHandlesIPv6AndMissingPort(t *testing.T) {
	if got := ClientIP(reqWith("[2001:db8::1]:443", ""), 0); got != "2001:db8::1" {
		t.Errorf("IPv6: ClientIP = %q", got)
	}
	if got := ClientIP(reqWith("203.0.113.9", ""), 0); got != "203.0.113.9" {
		t.Errorf("ไม่มี port: ClientIP = %q", got)
	}
}

func TestIsAllowed(t *testing.T) {
	list := []string{"203.0.113.9", "198.51.100.1"}
	cases := []struct {
		ip       string
		allowAll bool
		allowed  []string
		want     bool
	}{
		{"203.0.113.9", false, list, true},
		{"1.2.3.4", false, list, false},
		{"1.2.3.4", true, nil, true},
		{"203.0.113.9", false, nil, false}, // ว่าง = ปิดทุกคน
		{"", false, list, false},
	}
	for _, c := range cases {
		if got := IsAllowed(c.ip, c.allowed, c.allowAll); got != c.want {
			t.Errorf("IsAllowed(%q, %v, %v) = %v, want %v", c.ip, c.allowed, c.allowAll, got, c.want)
		}
	}
}
```

- [ ] **Step 2: เขียน clientip.go**

```go
package httpapi

import (
	"net"
	"net/http"
	"strings"
)

// ClientIP หา IP ของ caller จริงโดยนับจากขวาของ chain
//
// chain = ค่าใน X-Forwarded-For ต่อท้ายด้วย RemoteAddr
// ตัวขวาสุดคือ peer ที่ต่อเข้ามาจริง แต่ละ proxy ที่เราไว้ใจจะ append IP ของขาเข้ามันเอง
// ดังนั้น caller จริงอยู่ที่ตำแหน่งที่ (trustedProxies + 1) นับจากขวา
//
// ระบบเก่าใช้ตัวซ้ายสุด (controllers/homeController.go:75) ซึ่ง caller ใส่อะไรก็ได้
func ClientIP(r *http.Request, trustedProxies int) string {
	var chain []string
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for _, p := range strings.Split(xff, ",") {
			if p = strings.TrimSpace(p); p != "" {
				chain = append(chain, p)
			}
		}
	}
	chain = append(chain, hostOnly(r.RemoteAddr))

	idx := len(chain) - 1 - trustedProxies
	if idx < 0 {
		idx = 0
	}
	return chain[idx]
}

func hostOnly(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return strings.TrimSpace(addr)
}

// IsAllowed ตรวจ whitelist — รายการว่างแปลว่าปิดทุกคน ต้องใส่ "*" ถึงจะเปิด
func IsAllowed(ip string, allowed []string, allowAll bool) bool {
	if allowAll {
		return true
	}
	if ip == "" {
		return false
	}
	for _, a := range allowed {
		if a == ip {
			return true
		}
	}
	return false
}
```

- [ ] **Step 3: รัน test ให้ผ่าน**

Run: `go test ./internal/httpapi/ -v`
Expected: PASS ทุกเคส

- [ ] **Step 4: เขียน test ของ handler**

สร้าง `internal/httpapi/handler_test.go`:

```go
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/celalsahinaltinisik/internal/amqpx"
	"github.com/celalsahinaltinisik/internal/config"
	"github.com/celalsahinaltinisik/internal/flow"
	"github.com/celalsahinaltinisik/internal/model"
	"github.com/celalsahinaltinisik/internal/store"
	amqp "github.com/rabbitmq/amqp091-go"
)

type fakeLogger struct {
	begun    []store.BeginRequestInput
	outcomes map[string]int
}

func newFakeLogger() *fakeLogger { return &fakeLogger{outcomes: map[string]int{}} }

func (f *fakeLogger) BeginRequest(_ context.Context, in store.BeginRequestInput) error {
	f.begun = append(f.begun, in)
	return nil
}

func (f *fakeLogger) MarkClientOutcome(_ context.Context, traceID string, status int) error {
	f.outcomes[traceID] = status
	return nil
}

type fakeCaller struct {
	reply *amqp.Delivery
	err   error
	queue string
	pub   amqp.Publishing
}

func (f *fakeCaller) Call(_ context.Context, queue string, pub amqp.Publishing) (*amqp.Delivery, error) {
	f.queue, f.pub = queue, pub
	return f.reply, f.err
}

type stubBroker struct{ msgs chan amqp.Delivery }

func (b *stubBroker) DeclareQueue(string) error { return nil }
func (b *stubBroker) Consume(string, int) (<-chan amqp.Delivery, string, error) {
	return b.msgs, "t", nil
}
func (b *stubBroker) Cancel(string) error { return nil }
func (b *stubBroker) Close() error        { return nil }

func registryWith(name string, st flow.State, urls ...string) *flow.Registry {
	spec := model.GroupSpec{ID: "g1", Name: name, WorkerCount: 2,
		RPCTimeout: 30 * time.Second, RefField: "customer_order_id"}
	for i, u := range urls {
		spec.URLs = append(spec.URLs, model.URLSpec{ID: int64(i + 1), URL: u})
	}
	f := flow.New(flow.Options{Spec: spec, Queue: "v2." + name,
		Broker:  &stubBroker{msgs: make(chan amqp.Delivery)},
		Process: func(context.Context, amqp.Delivery, model.GroupSpec) {}})
	r := flow.NewRegistry()
	r.Put("g1", f, st)
	return r
}

func testHandler(reg *flow.Registry, lg *fakeLogger, caller *fakeCaller) *Handler {
	return New(Options{
		Registry: reg,
		Logger:   lg,
		Caller:   caller,
		Cfg: &config.Config{QueuePrefix: "v2.", AllowAllIPs: true,
			TrustedProxyCount: 0},
		NewID: func() string { return "trace-fixed" },
	})
}

func post(h *Handler, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.RemoteAddr = "203.0.113.9:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func okReply(status int, body string) *amqp.Delivery {
	return &amqp.Delivery{
		Headers: amqp.Table{amqpx.HeaderUpstreamStatus: int32(status)},
		Body:    []byte(body),
	}
}

func TestPostForwardsAndReturnsTraceHeader(t *testing.T) {
	lg, caller := newFakeLogger(), &fakeCaller{reply: okReply(200, `{"code":0}`)}
	h := testHandler(registryWith("withdraw", flow.StateRunning, "https://a"), lg, caller)

	w := post(h, "/withdraw", `{"customer_order_id":"ORDER-1"}`)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != `{"code":0}` {
		t.Errorf("body = %q — ต้องเป็น passthrough", w.Body.String())
	}
	if w.Header().Get("X-Trace-Id") != "trace-fixed" {
		t.Error("ต้องคืน X-Trace-Id ให้ support ไล่ปัญหาจากที่ลูกค้าแจ้งได้")
	}
	if caller.queue != "v2.withdraw" {
		t.Errorf("queue = %q, want v2.withdraw — prefix ต้องถูกใส่", caller.queue)
	}
	if len(lg.begun) != 1 || lg.begun[0].BusinessRef != "ORDER-1" {
		t.Errorf("BeginRequest = %+v — ต้องดึง business_ref จาก body", lg.begun)
	}
	if lg.outcomes["trace-fixed"] != 200 {
		t.Errorf("MarkClientOutcome = %d, want 200", lg.outcomes["trace-fixed"])
	}
}

func TestUpstreamStatusIsPassedThrough(t *testing.T) {
	lg, caller := newFakeLogger(), &fakeCaller{reply: okReply(400, `{"message":"ยอดเงินไม่พอ"}`)}
	h := testHandler(registryWith("withdraw", flow.StateRunning, "https://a"), lg, caller)

	w := post(h, "/withdraw", `{}`)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400 ตาม spec §7.7", w.Code)
	}
	if lg.outcomes["trace-fixed"] != 400 {
		t.Errorf("http_status ที่บันทึก = %d, want 400", lg.outcomes["trace-fixed"])
	}
}

func TestPublishesTraceAndDeadlineHeaders(t *testing.T) {
	lg, caller := newFakeLogger(), &fakeCaller{reply: okReply(200, `{}`)}
	h := testHandler(registryWith("withdraw", flow.StateRunning, "https://a"), lg, caller)

	post(h, "/withdraw", `{}`)

	if caller.pub.Headers[flow.HeaderTraceID] != "trace-fixed" {
		t.Error("ต้องส่ง x-trace-id ไปกับข้อความ")
	}
	if caller.pub.Headers[flow.HeaderDeadline] == nil {
		t.Fatal("ต้องส่ง x-deadline ไปกับข้อความ ไม่งั้น worker จะยิง upstream หลัง caller เลิกรอ")
	}
	if caller.pub.CorrelationId == "" {
		t.Error("ต้องมี correlation_id")
	}
}

func TestRPCTimeoutReturns504AndRecordsIt(t *testing.T) {
	lg := newFakeLogger()
	caller := &fakeCaller{err: context.DeadlineExceeded}
	h := testHandler(registryWith("withdraw", flow.StateRunning, "https://a"), lg, caller)

	w := post(h, "/withdraw", `{}`)
	if w.Code != 504 {
		t.Fatalf("status = %d, want 504", w.Code)
	}
	if lg.outcomes["trace-fixed"] != 504 {
		t.Errorf("ต้องบันทึกว่า caller ได้ 504 ไว้ให้ไล่เคส 'caller คิดว่าล้มแต่ออเดอร์เกิดจริง'")
	}
}

func TestUnknownGroupIs404(t *testing.T) {
	h := testHandler(registryWith("withdraw", flow.StateRunning, "https://a"),
		newFakeLogger(), &fakeCaller{reply: okReply(200, `{}`)})
	if w := post(h, "/ไม่มีกลุ่มนี้", `{}`); w.Code != 404 {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestDrainingGroupIs503(t *testing.T) {
	h := testHandler(registryWith("withdraw", flow.StateDraining, "https://a"),
		newFakeLogger(), &fakeCaller{reply: okReply(200, `{}`)})
	w := post(h, "/withdraw", `{}`)
	if w.Code != 503 {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("503 ควรมี Retry-After")
	}
}

func TestGroupWithoutUpstreamIs503(t *testing.T) {
	h := testHandler(registryWith("withdraw", flow.StateDegraded),
		newFakeLogger(), &fakeCaller{reply: okReply(200, `{}`)})
	if w := post(h, "/withdraw", `{}`); w.Code != 503 {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestNonPostIs405(t *testing.T) {
	h := testHandler(registryWith("withdraw", flow.StateRunning, "https://a"),
		newFakeLogger(), &fakeCaller{reply: okReply(200, `{}`)})
	r := httptest.NewRequest(http.MethodGet, "/withdraw", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 405 {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

func TestBlockedIPIs403AndNeverPublishes(t *testing.T) {
	lg, caller := newFakeLogger(), &fakeCaller{reply: okReply(200, `{}`)}
	h := New(Options{
		Registry: registryWith("withdraw", flow.StateRunning, "https://a"),
		Logger:   lg, Caller: caller,
		Cfg:   &config.Config{QueuePrefix: "v2.", AllowedIPs: []string{"198.51.100.1"}},
		NewID: func() string { return "trace-fixed" },
	})

	w := post(h, "/withdraw", `{}`)
	if w.Code != 403 {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if caller.queue != "" {
		t.Error("IP ไม่ผ่านต้องไม่ publish อะไรเลย")
	}
	if len(lg.begun) != 0 {
		t.Error("IP ไม่ผ่านต้องไม่สร้างแถวใน request_logs")
	}
}

func TestHealthzAlwaysOK(t *testing.T) {
	h := testHandler(registryWith("withdraw", flow.StateFailed, "https://a"),
		newFakeLogger(), &fakeCaller{})
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("healthz = %d, want 200 — ต้องตอบได้แม้ flow ยังไม่ขึ้น", w.Code)
	}
}

func TestReadyzReflectsFlowState(t *testing.T) {
	h := testHandler(registryWith("withdraw", flow.StateFailed, "https://a"),
		newFakeLogger(), &fakeCaller{})
	r := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != 503 {
		t.Fatalf("readyz = %d, want 503 เมื่อมี flow ที่ failed", w.Code)
	}
	var body struct {
		Ready bool              `json:"ready"`
		Flows map[string]string `json:"flows"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("อ่าน body ไม่ได้: %v", err)
	}
	if body.Ready || body.Flows["withdraw"] != "failed" {
		t.Errorf("body = %+v — ต้องบอกเป็นรายตัวว่าใครไม่ขึ้น", body)
	}
}
```

- [ ] **Step 5: รัน test ให้เห็นว่า fail**

Run: `go test ./internal/httpapi/ -run 'TestPost|TestUpstream|TestPublishes|TestRPC|TestUnknown|TestDraining|TestGroupWithout|TestNonPost|TestBlocked|TestHealthz|TestReadyz' -v`
Expected: FAIL — ยังไม่มี `New` และ `Options`

- [ ] **Step 6: เขียน handler**

สร้าง `internal/httpapi/handler.go`:

```go
package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/celalsahinaltinisik/internal/amqpx"
	"github.com/celalsahinaltinisik/internal/config"
	"github.com/celalsahinaltinisik/internal/flow"
	"github.com/celalsahinaltinisik/internal/model"
	"github.com/celalsahinaltinisik/internal/store"
	amqp "github.com/rabbitmq/amqp091-go"
)

const maxRequestBytes = 4 * 1024 * 1024

type RequestLogger interface {
	BeginRequest(ctx context.Context, in store.BeginRequestInput) error
	MarkClientOutcome(ctx context.Context, traceID string, httpStatus int) error
}

type RPCCaller interface {
	Call(ctx context.Context, queue string, pub amqp.Publishing) (*amqp.Delivery, error)
}

type Options struct {
	Registry *flow.Registry
	Logger   RequestLogger
	Caller   RPCCaller
	Cfg      *config.Config
	NewID    func() string
	Now      func() time.Time
	Logf     func(string, ...any)
}

type Handler struct{ o Options }

func New(o Options) *Handler {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	return &Handler{o: o}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	case "/readyz":
		h.readyz(w)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ip := ClientIP(r, h.o.Cfg.TrustedProxyCount)
	if !IsAllowed(ip, h.o.Cfg.AllowedIPs, h.o.Cfg.AllowAllIPs) {
		h.o.Logf("🚫 ปฏิเสธ IP %s ที่ %s", ip, r.URL.Path)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	name := strings.Trim(r.URL.Path, "/")
	if name == "" || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}

	f, state, ok := h.o.Registry.ByName(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	spec := f.Spec()

	if state != flow.StateRunning || !spec.HasUpstream() {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "flow ยังไม่พร้อมรับงาน: "+string(state), http.StatusServiceUnavailable)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
	if err != nil {
		http.Error(w, "อ่าน body ไม่สำเร็จ", http.StatusBadRequest)
		return
	}

	traceID := h.o.NewID()
	w.Header().Set("X-Trace-Id", traceID)

	start := h.o.Now()
	deadline := start.Add(spec.RPCTimeout)

	beginErr := h.o.Logger.BeginRequest(r.Context(), store.BeginRequestInput{
		TraceID: traceID, GroupID: spec.ID, GroupName: spec.Name,
		CallerTraceID: r.Header.Get("X-Trace-Id"),
		ClientIP:      ip,
		Body:          body,
		BusinessRef:   model.ExtractRef(body, spec.RefField),
	})
	if beginErr != nil {
		// ถ้าบันทึกไม่ได้ก็ไม่ควรทำงานต่อ เพราะจะกลายเป็นออเดอร์ที่ไม่มีร่องรอย
		h.o.Logf("❌ BeginRequest ล้มเหลว trace=%s: %v", traceID, beginErr)
		http.Error(w, "ระบบบันทึกไม่พร้อม", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()

	headers := amqp.Table{
		flow.HeaderTraceID:  traceID,
		flow.HeaderDeadline: strconv.FormatInt(deadline.UnixMilli(), 10),
	}
	for k, vs := range r.Header {
		if len(vs) > 0 && !hopByHopRequest[http.CanonicalHeaderKey(k)] {
			headers[k] = vs[0]
		}
	}

	reply, err := h.o.Caller.Call(ctx, spec.QueueName(h.o.Cfg.QueuePrefix), amqp.Publishing{
		ContentType:   "application/json",
		CorrelationId: traceID,
		Headers:       headers,
		Body:          body,
	})
	if err != nil {
		h.o.Logf("⏱  ไม่ได้รับ reply trace=%s: %v", traceID, err)
		h.mark(r.Context(), traceID, http.StatusGatewayTimeout)
		http.Error(w, "upstream ไม่ตอบกลับในเวลาที่กำหนด", http.StatusGatewayTimeout)
		return
	}

	status := upstreamStatus(reply)
	h.mark(r.Context(), traceID, status)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(reply.Body)
}

func (h *Handler) mark(ctx context.Context, traceID string, status int) {
	if err := h.o.Logger.MarkClientOutcome(ctx, traceID, status); err != nil {
		h.o.Logf("⚠️  บันทึกผลฝั่ง caller ไม่สำเร็จ trace=%s: %v", traceID, err)
	}
}

func (h *Handler) readyz(w http.ResponseWriter) {
	ready, states := h.o.Registry.AllRunning()
	out := map[string]string{}
	for name, st := range states {
		out[name] = string(st)
	}
	w.Header().Set("Content-Type", "application/json")
	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ready": ready, "flows": out})
}

func upstreamStatus(d *amqp.Delivery) int {
	if d == nil || d.Headers == nil {
		return http.StatusOK
	}
	switch v := d.Headers[amqpx.HeaderUpstreamStatus].(type) {
	case int32:
		return int(v)
	case int64:
		return int(v)
	case int:
		return v
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return http.StatusOK
}

var hopByHopRequest = map[string]bool{
	"Connection":        true,
	"Keep-Alive":        true,
	"Transfer-Encoding": true,
	"Upgrade":           true,
	"Host":              true,
	"Content-Length":    true,
}
```

- [ ] **Step 7: รัน test ให้ผ่าน**

Run: `go test ./internal/... -race`
Expected: PASS ทุกแพ็กเกจ

- [ ] **Step 8: Commit**

```bash
git add internal/httpapi/
git commit -m "feat(httpapi): route จาก registry, whitelist ที่ปลอมไม่ได้, healthz/readyz"
```

---

## Task 13: ประกอบทั้งหมด + Dockerfile

**Files:**
- Create: `internal/amqpx/broker.go`
- Create: `cmd/gateway/main.go`
- Create: `Dockerfile.gateway`, `.dockerignore`
- Modify: `.gitignore`

**Interfaces:**
- Consumes: ทุกแพ็กเกจจาก Task 1-12
- Produces: `amqpx.NewBroker(ctx, m *Manager) (*Broker, error)` ที่ implement `flow.Broker` พร้อม `Publish`

- [ ] **Step 1: เขียน broker adapter**

สร้าง `internal/amqpx/broker.go`:

```go
package amqpx

import (
	"context"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Broker คือ channel หนึ่งช่องที่ flow หนึ่งตัวใช้
// ต้องแยกช่องต่อ flow เพราะ Qos (prefetch) เป็นค่าระดับ channel
type Broker struct {
	ch *amqp.Channel
}

func NewBroker(ctx context.Context, m *Manager) (*Broker, error) {
	ch, err := m.Channel(ctx)
	if err != nil {
		return nil, err
	}
	return &Broker{ch: ch}, nil
}

// DeclareQueue ประกาศ queue แบบ durable
//
// durable=true ให้ตัวนิยาม queue รอดข้าม broker restart ซึ่งถูกมากเพราะไม่มี fsync ต่อข้อความ
// ส่วนข้อความไม่ตั้ง persistent โดยตั้งใจ — ถ้า broker restart ข้อความที่รอดมาก็เลย
// x-deadline ไปแล้วทั้งหมด การเก็บมันไว้จึงไม่มีประโยชน์
func (b *Broker) DeclareQueue(name string) error {
	_, err := b.ch.QueueDeclare(name, true, false, false, false, nil)
	return err
}

func (b *Broker) Consume(queue string, prefetch int) (<-chan amqp.Delivery, string, error) {
	if err := b.ch.Qos(prefetch, 0, false); err != nil {
		return nil, "", err
	}
	tag := "gw-" + queue
	msgs, err := b.ch.Consume(queue, tag, false, false, false, false, nil)
	if err != nil {
		return nil, "", err
	}
	return msgs, tag, nil
}

// Cancel สั่ง broker หยุดส่งข้อความใหม่ — msgs จะปิดเองหลัง delivery สุดท้าย
func (b *Broker) Cancel(tag string) error { return b.ch.Cancel(tag, false) }

func (b *Broker) Publish(ctx context.Context, queue string, pub amqp.Publishing) error {
	return b.ch.PublishWithContext(ctx, "", queue, false, false, pub)
}

func (b *Broker) Close() error { return b.ch.Close() }
```

- [ ] **Step 2: เขียน main**

สร้าง `cmd/gateway/main.go`:

```go
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/celalsahinaltinisik/internal/amqpx"
	"github.com/celalsahinaltinisik/internal/config"
	"github.com/celalsahinaltinisik/internal/flow"
	"github.com/celalsahinaltinisik/internal/forward"
	"github.com/celalsahinaltinisik/internal/httpapi"
	"github.com/celalsahinaltinisik/internal/model"
	"github.com/celalsahinaltinisik/internal/reconcile"
	"github.com/celalsahinaltinisik/internal/store"
	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	// 1. config — ขาดอะไรตายตรงนี้พร้อมบอกชื่อตัวแปร
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		log.Fatalf("❌ %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 2-3. DB + migration
	db, err := store.Open(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("❌ เปิด DB ไม่ได้: %v", err)
	}
	defer db.Close()

	pingCtx, pingCancel := context.WithTimeout(ctx, 10*time.Second)
	if err := db.PingContext(pingCtx); err != nil {
		pingCancel()
		log.Fatalf("❌ ต่อ DB ไม่ได้: %v", err)
	}
	pingCancel()

	if cfg.MigrateOnStart {
		if err := store.Migrate(ctx, db); err != nil {
			log.Fatalf("❌ migration ล้มเหลว: %v", err)
		}
		log.Printf("✅ migration เรียบร้อย")
	}
	st := store.New(db)

	// 4. RabbitMQ
	mgr := amqpx.NewManager(cfg.RabbitMQURL)
	mgr.Logf = log.Printf
	defer mgr.Close()

	pool, err := amqpx.NewRPCPool(ctx, mgr, cfg.RPCChannelPool)
	if err != nil {
		log.Fatalf("❌ สร้าง RPC pool ไม่ได้: %v", err)
	}
	defer pool.Close()
	log.Printf("✅ ต่อ RabbitMQ แล้ว (pool %d ช่อง, prefix %q)", cfg.RPCChannelPool, cfg.QueuePrefix)

	// 5. registry + factory
	registry := flow.NewRegistry()
	fwd := forward.New()

	factory := func(spec model.GroupSpec) (*flow.Flow, error) {
		broker, err := amqpx.NewBroker(ctx, mgr)
		if err != nil {
			return nil, err
		}
		proc := flow.NewProcessor(fwd, st,
			func(ctx context.Context, replyTo string, pub amqp.Publishing) error {
				return broker.Publish(ctx, replyTo, pub)
			})
		proc.Logf = log.Printf

		return flow.New(flow.Options{
			Spec:    spec,
			Queue:   spec.QueueName(cfg.QueuePrefix),
			Broker:  broker,
			Process: proc.Handle,
			Logf:    log.Printf,
		}), nil
	}

	loop := &reconcile.Loop{
		Loader: st, Registry: registry, Factory: factory,
		Interval: cfg.ReconcileInterval,
		Drain:    cfg.GracefulTimeout,
		Logf:     log.Printf,
	}

	// 6. HTTP ขึ้นก่อน เพื่อให้ /healthz ตอบได้ระหว่างบูต
	handler := httpapi.New(httpapi.Options{
		Registry: registry, Logger: st, Caller: pool, Cfg: cfg,
		NewID: uuid.NewString, Logf: log.Printf,
	})
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("🌐 ฟังอยู่ที่ :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("❌ ListenAndServe: %v", err)
		}
	}()

	// 7. reconcile รอบแรกแบบ sync — flow ทุกตัวขึ้นตรงนี้ ไม่ต้อง curl อะไรทั้งนั้น
	if err := loop.Once(ctx); err != nil {
		log.Printf("⚠️  reconcile รอบแรกล้มเหลว: %v (จะลองใหม่ในรอบถัดไป)", err)
	}
	warnIfTimeoutsExceedGrace(ctx, st, cfg.GracefulTimeout)

	// 8. reconcile loop
	go loop.Run(ctx)

	// graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	sig := <-stop
	log.Printf("🛑 ได้รับสัญญาณ %v — เริ่ม graceful shutdown", sig)

	cancel() // หยุด reconcile loop

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.GracefulTimeout)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("⚠️  ปิด HTTP server: %v", err)
	}

	// drain ทุก flow ขนานกัน — Cancel ก่อนเสมอเพื่อไม่ให้ดูดงานใหม่เข้ามา
	drainAll(registry, cfg.GracefulTimeout)
	log.Printf("👋 ปิดเรียบร้อย")
}

func drainAll(r *flow.Registry, timeout time.Duration) {
	snap := r.Snapshot()
	done := make(chan struct{}, len(snap))
	for id := range snap {
		go func(id string) {
			defer func() { done <- struct{}{} }()
			f, _, ok := r.Get(id)
			if !ok {
				return
			}
			if err := f.Drain(timeout); err != nil {
				log.Printf("⚠️  drain %s: %v", f.Spec().Name, err)
			}
		}(id)
	}
	for range snap {
		<-done
	}
}

// warnIfTimeoutsExceedGrace เตือนตั้งแต่ตอน start ถ้า config ทำให้ shutdown ไม่มีวันจบทัน
// ระบบเก่าต้องใช้ 30 วินาทีแต่ docker ให้ 10 วินาที จึงโดน SIGKILL ทุกครั้ง
func warnIfTimeoutsExceedGrace(ctx context.Context, st *store.Store, grace time.Duration) {
	groups, err := st.LoadGroups(ctx)
	if err != nil {
		return
	}
	for _, g := range groups {
		if g.RPCTimeout+10*time.Second > grace {
			log.Printf("⚠️  group %s: rpc_timeout %v + 10s เกิน GRACEFUL_TIMEOUT %v — "+
				"ตอน deploy งานที่ค้างจะถูกตัดกลางคัน", g.Name, g.RPCTimeout, grace)
		}
		urlCount := len(g.URLs)
		if urlCount == 0 {
			urlCount = 1
		}
		if g.UpstreamTimeout*time.Duration(urlCount) > g.RPCTimeout {
			log.Printf("⚠️  group %s: upstream_timeout %v × %d url เกิน rpc_timeout %v — "+
				"caller อาจได้ 504 ก่อนที่ระบบจะลอง url ครบ",
				g.Name, g.UpstreamTimeout, len(g.URLs), g.RPCTimeout)
		}
	}
}

```

- [ ] **Step 3: ยืนยันว่าทุกอย่างคอมไพล์และ test ผ่าน**

Run: `go build ./... && go vet ./... && go test ./... -race`
Expected: PASS ทั้งหมด (integration test ยัง skip เพราะไม่ได้ตั้ง build tag)

- [ ] **Step 4: เขียน .dockerignore และ Dockerfile.gateway**

สร้าง `.dockerignore` — `.env` ของระบบเก่ามีรหัส Postgres จริงและตอนนี้ถูก `COPY . .` เข้า image ทุกครั้ง:

```
.git
.env
.DS_Store
CLAUDE.local.md
docs
*.md
Dockerfile
Dockerfile.gateway
docker-compose.yaml
```

สร้าง `Dockerfile.gateway`:

```dockerfile
# ---- build ----
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOTOOLCHAIN=local \
    go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

# ---- runtime ----
FROM alpine:3.20
# ca-certificates จำเป็นสำหรับต่อ https ไป upstream, tzdata เพราะ DSN ตั้ง TimeZone=Asia/Bangkok
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 app
COPY --from=build /out/gateway /usr/local/bin/gateway
USER app
EXPOSE 4000
ENTRYPOINT ["/usr/local/bin/gateway"]
```

เพิ่มใน `.gitignore`:

```
.env
.DS_Store
```

> `.env` ถูก track อยู่ใน git ตั้งแต่ commit `2daedcd` การเพิ่มใน `.gitignore` ไม่ได้เอามันออกจาก history
> การถอนออก (`git rm --cached .env`) และ **rotate รหัส Postgres** เป็นงานแยกตาม spec §9.4 ไม่อยู่ใน task นี้

- [ ] **Step 5: ยืนยันว่า image build ได้จริง**

ถ้าเครื่องมี docker:

```bash
docker build -f Dockerfile.gateway -t mq-gateway:dev .
docker run --rm mq-gateway:dev 2>&1 | head -3
```

Expected: ตายพร้อมข้อความ `❌ config ไม่ถูกต้อง: RABBITMQ_URL ต้องตั้งค่า; DATABASE_URL ต้องตั้งค่า; QUEUE_PREFIX ต้องตั้งค่า` — พิสูจน์ว่า binary รันได้และ validate config ถูกต้อง

ถ้าเครื่องไม่มี docker (เครื่อง dev ปัจจุบันไม่มี) ให้ยืนยันด้วยการ cross-compile แทน:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /tmp/gateway-linux ./cmd/gateway
ls -lh /tmp/gateway-linux
```

Expected: ได้ binary static ขนาดประมาณ 10-15MB แล้วให้ build image จริงบน Easypanel

- [ ] **Step 6: รัน smoke test กับของจริง**

ต้องมี Postgres และ RabbitMQ ที่เข้าถึงได้

```bash
export RABBITMQ_URL='amqp://guest:guest@...:5672/'
export DATABASE_URL='host=... dbname=mqv2 ...'
export QUEUE_PREFIX='v2.'
export ALLOWED_IPS='*'
go run ./cmd/gateway &
sleep 3

# ยังไม่มี group ในตาราง → readyz ต้องเขียว (ไม่มี flow ที่ล้ม) และ path ไหนก็ 404
curl -s localhost:4000/readyz
curl -s -o /dev/null -w '%{http_code}\n' -X POST localhost:4000/withdraw

# เพิ่ม group แล้วรอไม่เกิน 30 วิ (RECONCILE_INTERVAL)
psql "$DATABASE_URL" -c "
  WITH g AS (INSERT INTO message_group (group_name) VALUES ('smoke') RETURNING id)
  INSERT INTO message_group_url (message_group_id, url)
  SELECT id, 'https://httpbin.org/post' FROM g;"
sleep 35

curl -s localhost:4000/readyz
curl -s -D- -X POST localhost:4000/smoke -d '{"customer_order_id":"SMOKE-1"}'
```

Expected:
- ก่อนเพิ่ม group: `/readyz` ตอบ `{"ready":true,"flows":{}}`, POST `/withdraw` ได้ 404
- หลังเพิ่ม group: `/readyz` แสดง `{"smoke":"running"}` และ POST `/smoke` ได้ 200 พร้อม header `X-Trace-Id`
- **ไม่ต้อง curl อะไรเพื่อ start consumer เลย** — นี่คือ pain point ข้อ 4 ที่ถูกแก้
- ตรวจ log: `psql "$DATABASE_URL" -c "SELECT trace_id, group_name, status, http_status FROM request_logs;"` ต้องเห็นแถวที่ `status='success'`

- [ ] **Step 7: Commit**

```bash
git add internal/amqpx/broker.go cmd/ Dockerfile.gateway .dockerignore .gitignore
git commit -m "feat(gateway): ประกอบ service พร้อม Dockerfile multi-stage"
```

---

## หลังทำครบทุก task

1. รัน `go test ./... -race` และ `go test -tags=integration ./... -v` ให้ผ่านทั้งหมด
2. ทำตาม spec §8.5 ระยะที่ 1: deploy เป็น Easypanel service ใหม่ โดย **`QUEUE_PREFIX` ต้องไม่ว่าง** และ `message_group_url.url` ชี้ sandbox
3. ตรวจว่าไม่ได้ไปแย่ง queue ของระบบเก่า:
   ```bash
   rabbitmqctl list_queues name consumers -p /
   ```
   queue ชื่อ `withdraw`, `deposit`, `withdrawauto`, `depositauto` ต้องมี consumer เท่าเดิม
   และต้องเห็น queue ใหม่ชื่อ `v2.*` แยกออกมา
4. ตั้ง Easypanel: health check `/readyz`, stop timeout ≥ 60s
