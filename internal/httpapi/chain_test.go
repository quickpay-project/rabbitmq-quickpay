package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func snapOf(t *testing.T, r *http.Request) map[string]string {
	t.Helper()
	b := chainSnapshot(r)
	if b == nil {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("snapshot ไม่ใช่ JSON ที่ถูกต้อง: %v (%s)", err, b)
	}
	return m
}

// เก็บทุก header ที่อาจบรรจุ IP ไม่ได้เลือกใช้ตัวใดตัวหนึ่ง
// เพราะจุดประสงค์คือเห็นทุกอย่างที่มีตอนไล่ปัญหา
func TestChainSnapshotCapturesEverySource(t *testing.T) {
	r := httptest.NewRequest("POST", "/deposit", strings.NewReader("{}"))
	r.Host = "mq.goquickpay.com"
	r.RemoteAddr = "10.0.1.13:44321"
	r.Header.Set("X-Forwarded-For", "203.0.113.1, 104.22.176.7")
	r.Header.Set("CF-Connecting-IP", "203.0.113.1")
	r.Header.Set("CF-IPCountry", "TH")
	r.Header.Set("CF-Ray", "abc123-SIN")
	r.Header.Set("User-Agent", "Go-http-client/2.0")

	m := snapOf(t, r)
	for k, want := range map[string]string{
		"host": "mq.goquickpay.com", "remote": "10.0.1.13:44321",
		"xff": "203.0.113.1, 104.22.176.7", "cf_connecting_ip": "203.0.113.1",
		"cf_ipcountry": "TH", "cf_ray": "abc123-SIN", "ua": "Go-http-client/2.0",
	} {
		if m[k] != want {
			t.Errorf("%s = %q อยากได้ %q", k, m[k], want)
		}
	}
}

// ฟิลด์ที่ไม่มีค่าไม่ควรโผล่ ไม่งั้นทุกแถวจะมีคีย์ว่างเต็มไปหมด
func TestChainSnapshotOmitsEmpty(t *testing.T) {
	r := httptest.NewRequest("POST", "/deposit", strings.NewReader("{}"))
	r.Host = "h"
	r.RemoteAddr = "1.2.3.4:1"
	m := snapOf(t, r)
	for _, k := range []string{"cf_connecting_ip", "cf_ray", "x_real_ip", "true_client_ip"} {
		if _, ok := m[k]; ok {
			t.Errorf("ไม่ควรมีคีย์ %q เมื่อ header ไม่มีค่า", k)
		}
	}
}

// ค่ามาจาก header ที่ caller ควบคุมได้ ถ้าไม่ตัดจะกลายเป็นช่องยัดขยะลง DB
// ผ่านคำขอที่ถูกปฏิเสธไปแล้ว ซึ่งเป็นคำขอที่เราควบคุมต้นทางไม่ได้เลย
func TestChainSnapshotTruncatesLongValues(t *testing.T) {
	r := httptest.NewRequest("POST", "/deposit", strings.NewReader("{}"))
	r.Header.Set("User-Agent", strings.Repeat("A", 5000))
	m := snapOf(t, r)
	if len(m["ua"]) != maxChainValue {
		t.Fatalf("อยากได้ยาว %d แต่ได้ %d", maxChainValue, len(m["ua"]))
	}
}

// ไบต์ศูนย์ใน header ทำให้ JSONB ปฏิเสธทั้งแถว ต้องถูกล้างก่อน
// เป็นบั๊กแบบเดียวกับที่เคยเจอใน JSONOrRaw
func TestChainSnapshotSurvivesNulByte(t *testing.T) {
	r := httptest.NewRequest("POST", "/deposit", strings.NewReader("{}"))
	r.Header.Set("User-Agent", "abc"+string(rune(0))+"def")
	b := chainSnapshot(r)
	if strings.Contains(string(b), `\u0000`) {
		t.Fatalf("ยังมี escape ของไบต์ศูนย์ JSONB จะปฏิเสธ: %s", b)
	}
	m := snapOf(t, r)
	if m["ua"] != "abcdef" {
		t.Fatalf("ua = %q", m["ua"])
	}
}

// คำขอที่ไม่มีอะไรเลยต้องคืน nil เพื่อให้เก็บเป็น NULL ไม่ใช่ {} ที่ไม่มีความหมาย
func TestChainSnapshotNilWhenNothing(t *testing.T) {
	r := &http.Request{Header: http.Header{}}
	if b := chainSnapshot(r); b != nil {
		t.Fatalf("อยากได้ nil แต่ได้ %s", b)
	}
}

// handler ต้องแนบ chain ไปกับทุกครั้งที่ปฏิเสธ
func TestForbiddenRecordsChain(t *testing.T) {
	fb := &fakeBlocked{}
	h := denyingHandler(fb, "")
	r := httptest.NewRequest("POST", "/deposit", strings.NewReader("{}"))
	r.Host = "mq.goquickpay.com"
	r.RemoteAddr = "1.2.3.4:5555"
	r.Header.Set("CF-Connecting-IP", "203.0.113.77")
	h.ServeHTTP(httptest.NewRecorder(), r)

	if len(fb.got) != 1 {
		t.Fatalf("อยากได้ 1 รายการ แต่ได้ %d", len(fb.got))
	}
	var m map[string]string
	if err := json.Unmarshal(fb.got[0].Chain, &m); err != nil {
		t.Fatalf("chain ไม่ใช่ JSON: %v", err)
	}
	if m["host"] != "mq.goquickpay.com" || m["cf_connecting_ip"] != "203.0.113.77" {
		t.Fatalf("chain = %+v", m)
	}
}
