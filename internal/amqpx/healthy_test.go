package amqpx

import "testing"

// Manager ที่ยังไม่เคย dial ต้องรายงานว่าไม่พร้อม — ทดสอบได้โดยไม่ต้องมี broker จริง
// (ไม่มีการ dial เกิดขึ้นในเทสต์นี้ Healthy อ่านแค่สถานะ conn ที่ถืออยู่)
func TestManagerHealthyIsFalseBeforeAnyConnection(t *testing.T) {
	m := NewManager("amqp://guest:guest@127.0.0.1:1/")
	if m.Healthy() {
		t.Fatal("Manager ที่ยังไม่มี connection ต้องคืน false — ไม่งั้น readyz จะเขียวตอนบูตทั้งที่ยังต่อไม่ได้")
	}
}
