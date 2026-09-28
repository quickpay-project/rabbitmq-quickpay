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
	got := ClientIP(reqWith("203.0.113.9:5555", "1.2.3.4"), 0, "")
	if got != "203.0.113.9" {
		t.Fatalf("ClientIP = %q, want 203.0.113.9 — ไม่มี proxy ต้องไม่เชื่อ XFF เลย", got)
	}
}

// นี่คือบั๊กของระบบเก่า: homeController.go:75 เอาตัวซ้ายสุดซึ่ง caller ปลอมได้
func TestClientIPCountsFromRightSoSpoofingFails(t *testing.T) {
	// attacker ยิงผ่าน proxy พร้อมแนบ XFF ปลอม proxy จะ append IP จริงต่อท้าย
	got := ClientIP(reqWith("10.0.0.5:443", "9.9.9.9, 203.0.113.9"), 1, "")
	if got != "203.0.113.9" {
		t.Fatalf("ClientIP = %q, want 203.0.113.9 — ค่าที่ attacker ใส่เองต้องถูกข้าม", got)
	}
}

func TestClientIPWithTwoProxies(t *testing.T) {
	got := ClientIP(reqWith("10.0.0.5:443", "9.9.9.9, 203.0.113.9, 10.0.0.9"), 2, "")
	if got != "203.0.113.9" {
		t.Fatalf("ClientIP = %q, want 203.0.113.9", got)
	}
}

func TestClientIPFallsBackWhenChainShorterThanExpected(t *testing.T) {
	got := ClientIP(reqWith("10.0.0.5:443", ""), 2, "")
	if got != "10.0.0.5" {
		t.Fatalf("ClientIP = %q, want 10.0.0.5", got)
	}
}

func TestClientIPHandlesIPv6AndMissingPort(t *testing.T) {
	if got := ClientIP(reqWith("[2001:db8::1]:443", ""), 0, ""); got != "2001:db8::1" {
		t.Errorf("IPv6: ClientIP = %q", got)
	}
	if got := ClientIP(reqWith("203.0.113.9", ""), 0, ""); got != "203.0.113.9" {
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

// เมื่อตั้ง header ที่เชื่อถือได้ไว้ ต้องอ่านจาก header นั้นตรง ๆ ไม่ต้องนับ hop
// การนับ hop เปราะ: เปลี่ยนโครงสร้าง proxy เมื่อไหร่ allowlist พังเงียบ ๆ ทันที
// ซึ่งเกิดจริงตอนสลับ domain มาอยู่หลัง Cloudflare (2026-09-28)
func TestClientIPUsesTrustedHeaderWhenSet(t *testing.T) {
	r := reqWith("10.0.0.5:443", "9.9.9.9, 8.8.8.8, 7.7.7.7")
	r.Header.Set("CF-Connecting-IP", "203.0.113.9")
	if got := ClientIP(r, 1, "CF-Connecting-IP"); got != "203.0.113.9" {
		t.Fatalf("อยากได้ 203.0.113.9 แต่ได้ %q", got)
	}
}

// จำนวน proxy ต้องไม่มีผลเลยเมื่อใช้ header — นี่คือเหตุผลทั้งหมดของฟีเจอร์นี้
func TestClientIPTrustedHeaderIgnoresProxyCount(t *testing.T) {
	for _, n := range []int{0, 1, 2, 5, 99} {
		r := reqWith("10.0.0.5:443", "9.9.9.9, 8.8.8.8")
		r.Header.Set("CF-Connecting-IP", "203.0.113.9")
		if got := ClientIP(r, n, "CF-Connecting-IP"); got != "203.0.113.9" {
			t.Fatalf("proxy=%d: อยากได้ 203.0.113.9 แต่ได้ %q", n, got)
		}
	}
}

// header หายไป = คำขอไม่ได้ผ่าน proxy ที่เราประกาศว่าเชื่อถือ ต้องปฏิเสธ ไม่ใช่ถอยไปนับ hop
// ถ้าถอยไปนับ ใครที่ยิงเข้า origin ตรง ๆ จะปลอม X-Forwarded-For ผ่าน allowlist ได้ทันที
func TestClientIPTrustedHeaderMissingDeniesInsteadOfFallingBack(t *testing.T) {
	r := reqWith("10.0.0.5:443", "203.0.113.9")
	if got := ClientIP(r, 1, "CF-Connecting-IP"); got != "" {
		t.Fatalf("อยากได้ค่าว่างเพื่อให้ IsAllowed ปฏิเสธ แต่ได้ %q", got)
	}
	if IsAllowed("", []string{"203.0.113.9"}, false) {
		t.Fatal("ค่าว่างต้องไม่ผ่าน allowlist")
	}
}

// บาง proxy ส่งมาเป็นลิสต์ เอาตัวแรกซึ่งเป็น client จริง
func TestClientIPTrustedHeaderTakesFirstOfList(t *testing.T) {
	r := reqWith("10.0.0.5:443", "")
	r.Header.Set("True-Client-IP", " 203.0.113.9 , 10.0.0.1 ")
	if got := ClientIP(r, 1, "True-Client-IP"); got != "203.0.113.9" {
		t.Fatalf("อยากได้ 203.0.113.9 แต่ได้ %q", got)
	}
}

// ไม่ตั้ง header = พฤติกรรมเดิมทุกอย่าง ของเก่าต้องไม่กระทบ
func TestClientIPEmptyHeaderKeepsHopCounting(t *testing.T) {
	r := reqWith("10.0.0.5:443", "9.9.9.9, 203.0.113.9")
	r.Header.Set("CF-Connecting-IP", "1.1.1.1") // ต้องถูกเมิน เพราะไม่ได้ประกาศว่าเชื่อถือ
	if got := ClientIP(r, 1, ""); got != "203.0.113.9" {
		t.Fatalf("อยากได้ 203.0.113.9 (นับ hop เหมือนเดิม) แต่ได้ %q", got)
	}
}
