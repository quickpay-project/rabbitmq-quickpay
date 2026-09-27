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
