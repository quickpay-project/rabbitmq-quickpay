package amqpx

import (
	"testing"
	"time"
)

// Manager ที่ยังไม่เคย dial ต้องรายงานว่าไม่พร้อม — ทดสอบได้โดยไม่ต้องมี broker จริง
// (ไม่มีการ dial เกิดขึ้นในเทสต์นี้ Healthy อ่านแค่สถานะ conn ที่ถืออยู่)
func TestManagerHealthyIsFalseBeforeAnyConnection(t *testing.T) {
	m := NewManager("amqp://guest:guest@127.0.0.1:1/")
	if m.Healthy() {
		t.Fatal("Manager ที่ยังไม่มี connection ต้องคืน false — ไม่งั้น readyz จะเขียวตอนบูตทั้งที่ยังต่อไม่ได้")
	}
}

// Healthy ต้องไม่บล็อกเมื่อมี redial ค้างอยู่ — นี่คือช่วงเวลาที่ /readyz ต้องตอบให้ได้มากที่สุด
// ถ้า Healthy ใช้ Lock แทน TryLock เทสต์นี้จะค้างจนหมดเวลาแทนที่จะคืน false
func TestHealthyDoesNotBlockWhileRedialHoldsTheLock(t *testing.T) {
	m := NewManager("amqp://unused")

	// จำลองสภาพที่ connection() ถือ m.mu ค้างอยู่ระหว่าง retry loop
	m.mu.Lock()
	defer m.mu.Unlock()

	got := make(chan bool, 1)
	go func() { got <- m.Healthy() }()

	select {
	case healthy := <-got:
		if healthy {
			t.Fatal("ระหว่างมี redial ค้างอยู่ต้องรายงานว่าไม่ healthy")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Healthy บล็อกอยู่กับ mutex ที่ redial ถือค้าง — /readyz จะค้างแทนที่จะตอบ 503")
	}
}
