package reconcile

import (
	"testing"

	"github.com/celalsahinaltinisik/internal/flow"
	"github.com/celalsahinaltinisik/internal/model"
)

func spec(id, name string, workers int, urls ...string) model.GroupSpec {
	g := model.GroupSpec{ID: id, Name: name, WorkerCount: workers}
	for i, u := range urls {
		g.URLs = append(g.URLs, model.URLSpec{ID: int64(i + 1), URL: u})
	}
	return g
}

func entryOf(s model.GroupSpec, st flow.State) flow.Entry {
	return flow.Entry{ID: s.ID, Name: s.Name, Revision: s.Revision(),
		WorkerCount: s.WorkerCount, State: st}
}

func only(t *testing.T, actions []Action) Action {
	t.Helper()
	if len(actions) != 1 {
		t.Fatalf("actions = %d (%+v), want 1", len(actions), actions)
	}
	return actions[0]
}

func TestNewGroupProducesStart(t *testing.T) {
	s := spec("g1", "withdraw", 50, "https://a")
	a := only(t, Diff([]model.GroupSpec{s}, map[string]flow.Entry{}))
	if a.Kind != Start || a.Spec.Name != "withdraw" {
		t.Fatalf("action = %+v, want Start withdraw", a)
	}
}

func TestUnchangedGroupProducesNothing(t *testing.T) {
	s := spec("g1", "withdraw", 50, "https://a")
	got := Diff([]model.GroupSpec{s}, map[string]flow.Entry{"g1": entryOf(s, flow.StateRunning)})
	if len(got) != 0 {
		t.Fatalf("actions = %+v, want ว่าง", got)
	}
}

func TestURLChangeProducesHotSwap(t *testing.T) {
	old := spec("g1", "withdraw", 50, "https://a")
	next := spec("g1", "withdraw", 50, "https://a", "https://b")
	a := only(t, Diff([]model.GroupSpec{next}, map[string]flow.Entry{"g1": entryOf(old, flow.StateRunning)}))
	if a.Kind != HotSwap {
		t.Fatalf("action = %+v, want HotSwap — เปลี่ยน url ไม่ควรต้อง restart consumer", a)
	}
}

func TestWorkerCountChangeProducesRestart(t *testing.T) {
	old := spec("g1", "withdraw", 50, "https://a")
	next := spec("g1", "withdraw", 80, "https://a")
	a := only(t, Diff([]model.GroupSpec{next}, map[string]flow.Entry{"g1": entryOf(old, flow.StateRunning)}))
	if a.Kind != Restart {
		t.Fatalf("action = %+v, want Restart — prefetch ผูกกับ channel จึงต้องเปิดใหม่", a)
	}
}

func TestRenameProducesRestartNotStopAndStart(t *testing.T) {
	old := spec("g1", "withdraw", 50, "https://a")
	next := spec("g1", "withdraw-v2", 50, "https://a")
	a := only(t, Diff([]model.GroupSpec{next}, map[string]flow.Entry{"g1": entryOf(old, flow.StateRunning)}))
	if a.Kind != Restart {
		t.Fatalf("action = %+v, want Restart — UUID เดิมแปลว่าย้ายบ้าน ไม่ใช่ลบแล้วสร้างใหม่", a)
	}
}

func TestMissingFromDesiredProducesStop(t *testing.T) {
	old := spec("g1", "withdraw", 50, "https://a")
	a := only(t, Diff(nil, map[string]flow.Entry{"g1": entryOf(old, flow.StateRunning)}))
	if a.Kind != Stop || a.ID != "g1" {
		t.Fatalf("action = %+v, want Stop g1", a)
	}
}

func TestFailedFlowIsRestarted(t *testing.T) {
	s := spec("g1", "withdraw", 50, "https://a")
	a := only(t, Diff([]model.GroupSpec{s}, map[string]flow.Entry{"g1": entryOf(s, flow.StateFailed)}))
	if a.Kind != Restart {
		t.Fatalf("action = %+v, want Restart — flow ที่ failed ต้องถูกกู้", a)
	}
}

// Review Focus #5 — ชื่อผิดกติกาต้องไม่ทำให้ flow อื่นไม่ได้ขึ้น
func TestInvalidGroupNameIsSkippedWithoutBlockingOthers(t *testing.T) {
	bad := spec("g1", "Withdraw Prod", 50, "https://a")
	good := spec("g2", "deposit", 50, "https://b")

	actions := Diff([]model.GroupSpec{bad, good}, map[string]flow.Entry{})
	if len(actions) != 2 {
		t.Fatalf("actions = %d (%+v), want 2", len(actions), actions)
	}
	var sawSkip, sawStart bool
	for _, a := range actions {
		switch a.Kind {
		case Skip:
			sawSkip = true
			if a.Reason == "" {
				t.Error("Skip ต้องมีเหตุผลให้ log")
			}
		case Start:
			sawStart = true
			if a.Spec.Name != "deposit" {
				t.Errorf("ตัวที่ Start ควรเป็น deposit แต่เป็น %s", a.Spec.Name)
			}
		}
	}
	if !sawSkip || !sawStart {
		t.Fatalf("ต้องมีทั้ง Skip และ Start แต่ได้ %+v", actions)
	}
}

func TestReservedNameIsSkipped(t *testing.T) {
	a := only(t, Diff([]model.GroupSpec{spec("g1", "healthz", 50, "https://a")}, map[string]flow.Entry{}))
	if a.Kind != Skip {
		t.Fatalf("action = %+v, want Skip", a)
	}
}

// Regression — group ที่รันอยู่ดี ๆ แล้วมีคนไปแก้ชื่อใน DB ให้ผิดกติกา ต้องได้แค่ Skip
// ห้ามมี Stop โผล่มาเด็ดขาด ไม่งั้น apply จะ drain แล้ว remove consumer ที่กำลังรับออเดอร์จริง
// ทิ้งเพียงเพราะมีคนพิมพ์ชื่อผิด — ความเสียหายชนิดเดียวกับที่เคส rename กันไว้
func TestInvalidNameOnRunningFlowIsSkippedNotStopped(t *testing.T) {
	running := spec("g1", "withdraw", 50, "https://a")
	renamedBad := spec("g1", "Withdraw Prod", 50, "https://a")

	actions := Diff([]model.GroupSpec{renamedBad},
		map[string]flow.Entry{"g1": entryOf(running, flow.StateRunning)})

	for _, a := range actions {
		if a.Kind == Stop {
			t.Fatalf("เจอ Stop (%+v) — flow ที่รันอยู่ต้องไม่ถูกฆ่าเพราะชื่อใน DB ผิดกติกา", a)
		}
	}
	a := only(t, actions)
	if a.Kind != Skip {
		t.Fatalf("action = %+v, want Skip เท่านั้น", a)
	}
	if a.Reason == "" {
		t.Error("Skip ต้องมีเหตุผลให้ log")
	}
}
