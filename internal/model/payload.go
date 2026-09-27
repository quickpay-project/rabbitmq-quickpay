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
