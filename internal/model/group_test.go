package model

import "testing"

func TestQueueNameUsesPrefix(t *testing.T) {
	g := GroupSpec{Name: "withdraw"}
	if got := g.QueueName("v2."); got != "v2.withdraw" {
		t.Fatalf("QueueName = %q, want %q", got, "v2.withdraw")
	}
}

func TestRevisionStableAcrossURLOrder(t *testing.T) {
	a := GroupSpec{ID: "g1", Name: "withdraw", WorkerCount: 50,
		URLs: []URLSpec{{ID: 1, URL: "https://a"}, {ID: 2, URL: "https://b"}}}
	b := GroupSpec{ID: "g1", Name: "withdraw", WorkerCount: 50,
		URLs: []URLSpec{{ID: 2, URL: "https://b"}, {ID: 1, URL: "https://a"}}}
	if a.Revision() != b.Revision() {
		t.Fatal("Revision ต้องไม่ขึ้นกับลำดับของ URLs")
	}
}

func TestRevisionChangesWhenURLDeactivated(t *testing.T) {
	a := GroupSpec{ID: "g1", Name: "withdraw",
		URLs: []URLSpec{{ID: 1, URL: "https://a"}, {ID: 2, URL: "https://b"}}}
	b := GroupSpec{ID: "g1", Name: "withdraw",
		URLs: []URLSpec{{ID: 1, URL: "https://a"}}}
	if a.Revision() == b.Revision() {
		t.Fatal("Revision ต้องเปลี่ยนเมื่อ url หายไปหนึ่งตัว")
	}
}

func TestRevisionChangesWhenWorkerCountChanges(t *testing.T) {
	a := GroupSpec{ID: "g1", Name: "withdraw", WorkerCount: 50}
	b := GroupSpec{ID: "g1", Name: "withdraw", WorkerCount: 80}
	if a.Revision() == b.Revision() {
		t.Fatal("Revision ต้องเปลี่ยนเมื่อ worker_count เปลี่ยน")
	}
}

func TestValidateName(t *testing.T) {
	cases := []struct {
		name    string
		wantErr bool
	}{
		{"withdraw", false},
		{"withdraw-auto", false},
		{"deposit_v2", false},
		{"a", false},
		{"", true},
		{"Withdraw", true},  // ตัวใหญ่ไม่ได้ เพราะชื่อ queue และ path ต้องคาดเดาได้
		{"with draw", true}, // เว้นวรรคไม่ได้
		{"-withdraw", true}, // ห้ามขึ้นต้นด้วย -
		{"healthz", true},   // คำสงวน
		{"readyz", true},    // คำสงวน
		{"withdraw/../admin", true},
	}
	for _, c := range cases {
		err := GroupSpec{Name: c.name}.ValidateName()
		if (err != nil) != c.wantErr {
			t.Errorf("ValidateName(%q) err=%v, wantErr=%v", c.name, err, c.wantErr)
		}
	}
}

func TestValidateNameTooLong(t *testing.T) {
	long := ""
	for i := 0; i < 65; i++ {
		long += "a"
	}
	if err := (GroupSpec{Name: long}).ValidateName(); err == nil {
		t.Fatal("ชื่อยาว 65 ตัวอักษรต้องไม่ผ่าน")
	}
}

func TestActiveURLsEmptyMeansDegraded(t *testing.T) {
	g := GroupSpec{Name: "withdraw"}
	if g.HasUpstream() {
		t.Fatal("group ที่ไม่มี url ต้องรายงานว่าไม่มี upstream")
	}
}
