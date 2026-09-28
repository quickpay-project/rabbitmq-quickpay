package model

import (
	"bytes"
	"encoding/json"
	"testing"
)

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

func isValidJSON(b []byte) bool {
	var v any
	return jsonUnmarshal(b, &v) == nil
}

// JSONB ของ Postgres ปฏิเสธ escape ของไบต์ศูนย์ และไบต์ที่ไม่ใช่ UTF-8
// ทั้งที่ทั้งคู่เป็น JSON ที่ถูกต้องตามมาตรฐาน — body แบบนี้มาถึงได้จริง
// เพราะเราไม่บังคับว่า caller ต้องส่ง JSON ถ้าปล่อยผ่าน INSERT จะล้ม
// แล้ว handler ตอบ 503 ทั้งที่ควรแค่บันทึกไว้แล้วทำงานต่อ
func TestJSONOrRawStripsBytesJSONBRejects(t *testing.T) {
	zero := byte(0)
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"ไบต์ศูนย์", []byte{zero}},
		{"ไม่ใช่ UTF-8 ตามด้วยไบต์ศูนย์", []byte{0xff, zero}},
		{"ปนกับข้อความปกติ", append([]byte("abc"), zero, 0xff)},
	} {
		got := JSONOrRaw(tc.body)
		if !json.Valid(got) {
			t.Fatalf("%s: ผลลัพธ์ไม่ใช่ JSON ที่ถูกต้อง: %q", tc.name, got)
		}
		if bytes.Contains(got, []byte(`\u0000`)) {
			t.Fatalf("%s: ยังมี escape ของไบต์ศูนย์หลงเหลือ JSONB จะปฏิเสธ: %q", tc.name, got)
		}
		var m map[string]string
		if err := json.Unmarshal(got, &m); err != nil {
			t.Fatalf("%s: แกะกลับไม่ได้: %v", tc.name, err)
		}
		if _, ok := m["raw"]; !ok {
			t.Fatalf("%s: ต้องถูกห่อด้วยคีย์ raw แต่ได้ %q", tc.name, got)
		}
	}
}
