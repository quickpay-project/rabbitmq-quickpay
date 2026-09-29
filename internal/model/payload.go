package model

import (
	"encoding/json"
	"strconv"
	"strings"
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
	wrapped, err := json.Marshal(map[string]string{"raw": SafeForJSONB(string(body))})
	if err != nil {
		return []byte(`{"raw":""}`)
	}
	return wrapped
}

// SafeForJSONB ทำให้ string ใส่คอลัมน์ JSONB ได้เสมอ
//
// สองอย่างที่ JSONB ของ Postgres รับไม่ได้ ทั้งที่เป็น JSON ถูกต้องตามมาตรฐาน:
//   - ไบต์ 0x00 ซึ่งกลายเป็น escape \u0000 ตอน marshal แล้วโดนปฏิเสธด้วย
//     "pq: unsupported Unicode escape sequence"
//   - ไบต์ที่ไม่ใช่ UTF-8 ที่ถูกต้อง (json.Marshal แปลงให้เป็น U+FFFD อยู่แล้ว
//     แต่ทำตรงนี้ให้ชัดเจนว่าเป็นเจตนา ไม่ใช่ผลข้างเคียง)
//
// body แบบนี้มาถึงได้จริงเพราะเราไม่บังคับว่า caller ต้องส่ง JSON
// และถ้า INSERT ล้ม handler จะตอบ 503 "ระบบบันทึกไม่พร้อม" ทั้งที่ควรแค่บันทึกไว้แล้วทำงานต่อ
// เจอตอนรัน integration test ครั้งแรก ซึ่งไม่เคยถูกรันมาก่อนเพราะอยู่หลัง build tag
func SafeForJSONB(s string) string {
	s = strings.ToValidUTF8(s, string(rune(0xFFFD)))
	return strings.ReplaceAll(s, string(rune(0)), "")
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
