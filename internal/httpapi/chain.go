package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/celalsahinaltinisik/internal/model"
)

// maxChainValue จำกัดความยาวต่อฟิลด์ ค่าพวกนี้มาจาก header ที่ caller ควบคุมได้
// ปล่อยไว้จะกลายเป็นช่องให้ยัดข้อมูลขยะลง DB ผ่านคำขอที่ถูกปฏิเสธไปแล้ว
const maxChainValue = 256

// chainHeaders คือ header ทุกตัวที่อาจบรรจุ IP ของผู้เรียก เก็บไว้ดูทั้งหมด
// ไม่ได้เลือกใช้ตัวใดตัวหนึ่ง เพราะจุดประสงค์คือ "เห็นทุกอย่างที่มี" ตอนไล่ปัญหา
var chainHeaders = map[string]string{
	"xff":              "X-Forwarded-For",
	"cf_connecting_ip": "CF-Connecting-IP",
	"cf_ipcountry":     "CF-IPCountry",
	"cf_ray":           "CF-Ray",
	"x_real_ip":        "X-Real-IP",
	"true_client_ip":   "True-Client-IP",
	"forwarded":        "Forwarded",
	"ua":               "User-Agent",
}

// chainSnapshot เก็บ IP ทุกตัวที่เห็นในคำขอนั้นเป็น JSON ไว้ตรวจสอบอย่างเดียว
//
// **ไม่ถูกใช้ตัดสินใจอะไรทั้งสิ้น** การตัดสินว่า client เป็นใครยังอยู่ที่ ClientIP เหมือนเดิม
//
// host กับ remote เป็นสองตัวที่บอกได้มากที่สุด: host บอกว่าคำขอเข้ามาทาง domain ไหน
// (ผ่าน Cloudflare หรืออ้อมเข้า origin ตรง ๆ) ส่วน remote คือ peer จริงที่ปลอมไม่ได้
// ส่วน cf_ray ใช้ไปเทียบกับ log ฝั่ง Cloudflare ได้โดยตรง
func chainSnapshot(r *http.Request) []byte {
	m := map[string]string{}
	put := func(k, v string) {
		if v == "" {
			return
		}
		if len(v) > maxChainValue {
			v = v[:maxChainValue]
		}
		// header มาจาก input ที่ไม่ไว้ใจเหมือน body — ไบต์ศูนย์ทำให้ JSONB ปฏิเสธทั้งแถว
		if v = model.SafeForJSONB(v); v != "" {
			m[k] = v
		}
	}
	put("host", r.Host)
	put("remote", r.RemoteAddr)
	for key, hdr := range chainHeaders {
		put(key, r.Header.Get(hdr))
	}
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}
