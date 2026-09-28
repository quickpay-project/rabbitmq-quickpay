package httpapi

import (
	"net"
	"net/http"
	"strings"
)

// ClientIP หา IP ของ caller จริง มีสองโหมด
//
// **โหมด header** เมื่อ trustedHeader ไม่ว่าง จะอ่านจาก header นั้นตรง ๆ ไม่นับ hop เลย
// ใช้กับ CDN ที่รับประกันค่าให้ เช่น CF-Connecting-IP ของ Cloudflare ซึ่งเขียนทับ
// ของที่ caller ส่งมาทุกครั้ง ปลอมไม่ได้
//
// ถ้าตั้ง header ไว้แล้วคำขอไม่มี header นั้น จะคืนค่าว่างเพื่อให้ IsAllowed ปฏิเสธ
// **ไม่ถอยกลับไปนับ hop** เพราะการถอยกลับคือช่องโหว่: คำขอที่ไม่ได้ผ่าน proxy ที่เรา
// ประกาศว่าเชื่อถือ แปลว่ามีคนยิงเข้า origin ตรง ๆ ได้ ซึ่งเขาจะปลอม X-Forwarded-For
// ผ่าน allowlist ได้ทันที การปฏิเสธไปเลยจึงเป็นทางเดียวที่ปลอดภัย
//
// **โหมดนับ hop** (trustedHeader ว่าง) chain = X-Forwarded-For ต่อท้ายด้วย RemoteAddr
// ตัวขวาสุดคือ peer ที่ต่อเข้ามาจริง แต่ละ proxy ที่เราไว้ใจจะ append IP ของขาเข้ามันเอง
// caller จริงจึงอยู่ที่ตำแหน่งที่ (trustedProxies + 1) นับจากขวา
//
// โหมดนับ hop เปราะโดยธรรมชาติ: ตั้งเลขผิดหรือโครงสร้าง proxy เปลี่ยนเมื่อไหร่
// allowlist จะพังเงียบ ๆ ทันที เกิดจริงตอนสลับ domain มาอยู่หลัง Cloudflare (2026-09-28)
// ทำให้ทุก request โดน 403 — ถ้ามี CDN ที่ให้ header มาอยู่แล้ว ใช้โหมด header ดีกว่าเสมอ
//
// ระบบเก่าใช้ตัวซ้ายสุด (controllers/homeController.go:75) ซึ่ง caller ใส่อะไรก็ได้
func ClientIP(r *http.Request, trustedProxies int, trustedHeader string) string {
	if trustedHeader != "" {
		v := strings.TrimSpace(r.Header.Get(trustedHeader))
		// บาง proxy ส่งมาเป็นลิสต์ ตัวแรกคือ client จริง
		if i := strings.IndexByte(v, ','); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		return v
	}

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
