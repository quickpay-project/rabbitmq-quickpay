package reconcile

import (
	"github.com/celalsahinaltinisik/internal/flow"
	"github.com/celalsahinaltinisik/internal/model"
)

type ActionKind string

const (
	Start   ActionKind = "start"
	HotSwap ActionKind = "hotswap"
	Restart ActionKind = "restart"
	Stop    ActionKind = "stop"
	Skip    ActionKind = "skip"
)

type Action struct {
	Kind   ActionKind
	ID     string
	Spec   model.GroupSpec
	Reason string
}

// Diff เทียบสิ่งที่ DB บอกว่าควรเป็น กับสิ่งที่รันอยู่จริง แล้วบอกว่าต้องทำอะไร
// เป็นฟังก์ชันบริสุทธิ์ทั้งหมดเพื่อให้ทดสอบทุกเคสได้โดยไม่ต้องมี broker หรือ DB
func Diff(desired []model.GroupSpec, actual map[string]flow.Entry) []Action {
	var actions []Action
	seen := map[string]bool{}

	for _, want := range desired {
		// ต้องทำก่อนทุกอย่างและไม่มีเงื่อนไข: id ที่ DB ยังบอกว่าต้องมี ถือว่า "เห็นแล้ว" เสมอ
		// แม้จะถูกข้ามด้วยเหตุชื่อผิด ถ้าตั้งบรรทัดนี้หลัง continue ลูปที่สองจะคิดว่ามันหายไปจาก
		// desired แล้วออก Stop ตามมา = drain แล้ว remove consumer ที่กำลังรับออเดอร์จริง
		// เพียงเพราะมีคนพิมพ์ชื่อผิดใน DB
		seen[want.ID] = true

		if err := want.ValidateName(); err != nil {
			// ข้ามตัวที่ชื่อใช้ไม่ได้ แต่ตัวอื่นต้องทำงานต่อได้ตามปกติ
			// ตัวที่รันอยู่แล้วจะถูกปล่อยไว้เฉย ๆ ไม่มี action อื่นตามมา
			actions = append(actions, Action{Kind: Skip, ID: want.ID, Spec: want, Reason: err.Error()})
			continue
		}

		have, running := actual[want.ID]
		switch {
		case !running:
			actions = append(actions, Action{Kind: Start, ID: want.ID, Spec: want})
		case have.State == flow.StateFailed:
			actions = append(actions, Action{Kind: Restart, ID: want.ID, Spec: want,
				Reason: "flow อยู่ในสถานะ failed"})
		case have.Revision == want.Revision():
			// ไม่มีอะไรเปลี่ยน
		case have.WorkerCount != want.WorkerCount:
			actions = append(actions, Action{Kind: Restart, ID: want.ID, Spec: want,
				Reason: "worker_count เปลี่ยน ต้องตั้ง prefetch ใหม่บน channel"})
		case have.Name != want.Name:
			actions = append(actions, Action{Kind: Restart, ID: want.ID, Spec: want,
				Reason: "group_name เปลี่ยน ต้องย้ายไป queue ใหม่"})
		default:
			actions = append(actions, Action{Kind: HotSwap, ID: want.ID, Spec: want})
		}
	}

	for id, have := range actual {
		if !seen[id] {
			actions = append(actions, Action{Kind: Stop, ID: id,
				Reason: "ไม่มีใน DB แล้ว", Spec: model.GroupSpec{Name: have.Name}})
		}
	}
	return actions
}
