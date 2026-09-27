package model

import "testing"

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
