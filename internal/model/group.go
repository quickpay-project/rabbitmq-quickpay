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
