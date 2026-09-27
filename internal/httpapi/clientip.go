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
