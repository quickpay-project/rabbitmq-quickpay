package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/celalsahinaltinisik/internal/config"
	"github.com/celalsahinaltinisik/internal/flow"
)

func getReadyz(h *Handler) (*httptest.ResponseRecorder, struct {
	Ready bool              `json:"ready"`
	Flows map[string]string `json:"flows"`
	AMQP  string            `json:"amqp"`
}) {
	r := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var body struct {
		Ready bool              `json:"ready"`
		Flows map[string]string `json:"flows"`
		AMQP  string            `json:"amqp"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w, body
}

func handlerWithAMQP(reg *flow.Registry, healthy bool) *Handler {
	return New(Options{
		Registry: reg,
		Logger:   newFakeLogger(),
		Caller:   &fakeCaller{},
		Cfg:      &config.Config{QueuePrefix: "v2.", AllowAllIPs: true},
		NewID:    func() string { return "trace-fixed" },

		AMQPHealthy: func() bool { return healthy },
	})
}

// M2 — flow ทุกตัว running แต่ connection ของ broker หลุด: RPC pool ไม่เคยต่อใหม่
// ทุก request จะได้ 504 ถาวร readyz ต้องไม่เขียว ไม่งั้น orchestrator จะ route
// traffic เข้าหลุมดำแทนที่จะ restart pod (spec §6.5)
func TestReadyzIs503WhenAMQPConnectionIsDown(t *testing.T) {
	h := handlerWithAMQP(registryWith("withdraw", flow.StateRunning, "https://a"), false)

	w, body := getReadyz(h)
	if w.Code != 503 {
		t.Fatalf("readyz = %d, want 503 — AMQP หลุดแล้ว gateway รับงานไม่ได้", w.Code)
	}
	if body.Ready {
		t.Error("ready ต้องเป็น false")
	}
	if body.AMQP != "down" {
		t.Errorf("amqp = %q, want down — ต้องบอกได้ว่าไม่พร้อมเพราะอะไร", body.AMQP)
	}
	if body.Flows["withdraw"] != "running" {
		t.Errorf("flows = %+v — flow ยัง running อยู่จริง ต้องรายงานตามจริง", body.Flows)
	}
}

// เคสเขียว: flow running + AMQP ปกติ (เดิมไม่มี test ครอบ readyz ตอน ready = true)
func TestReadyzIsOKWhenFlowsRunningAndAMQPUp(t *testing.T) {
	h := handlerWithAMQP(registryWith("withdraw", flow.StateRunning, "https://a"), true)

	w, body := getReadyz(h)
	if w.Code != 200 {
		t.Fatalf("readyz = %d, want 200", w.Code)
	}
	if !body.Ready || body.AMQP != "up" || body.Flows["withdraw"] != "running" {
		t.Errorf("body = %+v, want ready=true amqp=up withdraw=running", body)
	}
}

// AMQP ปกติแต่มี flow ล้ม — ยังต้อง 503 (เงื่อนไขสองข้อต้องผ่านทั้งคู่)
func TestReadyzIs503WhenFlowFailedEvenIfAMQPUp(t *testing.T) {
	h := handlerWithAMQP(registryWith("withdraw", flow.StateFailed, "https://a"), true)

	w, body := getReadyz(h)
	if w.Code != 503 {
		t.Fatalf("readyz = %d, want 503", w.Code)
	}
	if body.AMQP != "up" || body.Flows["withdraw"] != "failed" {
		t.Errorf("body = %+v, want amqp=up withdraw=failed", body)
	}
}
