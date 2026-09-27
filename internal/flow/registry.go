package flow

import "sync"

type State string

const (
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateDegraded State = "degraded" // ไม่มี url ที่ active
	StateDraining State = "draining"
	StateFailed   State = "failed"
)

// Entry คือภาพนิ่งของ flow หนึ่งตัว ใช้ให้ reconciler เทียบกับ desired state
type Entry struct {
	ID          string
	Name        string
	Revision    string
	WorkerCount int
	State       State
}

type Registry struct {
	mu    sync.RWMutex
	flows map[string]*Flow
	state map[string]State
}

func NewRegistry() *Registry {
	return &Registry{flows: map[string]*Flow{}, state: map[string]State{}}
}

func (r *Registry) Put(id string, f *Flow, st State) {
	r.mu.Lock()
	r.flows[id] = f
	r.state[id] = st
	r.mu.Unlock()
}

func (r *Registry) SetState(id string, st State) {
	r.mu.Lock()
	if _, ok := r.flows[id]; ok {
		r.state[id] = st
	}
	r.mu.Unlock()
}

// SetStateIf เปลี่ยนสถานะเฉพาะเมื่อ f ยังเป็น flow ตัวปัจจุบันของ id นั้น คืน true เมื่อเปลี่ยนจริง
//
// จำเป็นเพราะตอน Restart มี flow สองตัวใช้ id เดียวกันในช่วงสั้น ๆ (stop ตัวเก่า แล้ว start ตัวใหม่)
// goroutine ของตัวเก่าที่กำลังตายอยู่ต้องไม่ไป mark ตัวใหม่เป็น failed — SetState ธรรมดา
// เช็คแค่ว่า id มีอยู่ ไม่เช็ค identity จึงเขียนทับสถานะของ flow ที่ไม่ใช่ตัวเองได้
// ผลของการไม่เช็คคือ Restart ปลอมทุกรอบเวลา config เปลี่ยนบ่อย ซึ่งแต่ละครั้ง
// คือการ drain consumer ที่แข็งแรงดีทิ้ง
func (r *Registry) SetStateIf(id string, f *Flow, st State) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.flows[id]; !ok || cur != f {
		return false
	}
	r.state[id] = st
	return true
}

func (r *Registry) Remove(id string) {
	r.mu.Lock()
	delete(r.flows, id)
	delete(r.state, id)
	r.mu.Unlock()
}

func (r *Registry) Get(id string) (*Flow, State, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.flows[id]
	return f, r.state[id], ok
}

// ByName ใช้จากฝั่ง HTTP เพื่อหา flow จาก path
func (r *Registry) ByName(name string) (*Flow, State, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for id, f := range r.flows {
		if f.Spec().Name == name {
			return f, r.state[id], true
		}
	}
	return nil, "", false
}

func (r *Registry) Snapshot() map[string]Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]Entry, len(r.flows))
	for id, f := range r.flows {
		s := f.Spec()
		out[id] = Entry{ID: id, Name: s.Name, Revision: s.Revision(),
			WorkerCount: s.WorkerCount, State: r.state[id]}
	}
	return out
}

// AllRunning ใช้ตอบ /readyz — degraded ถือว่ายัง ready เพราะ service ทำงานถูกต้องแล้ว
// แค่ไม่มีปลายทางให้ยิง ซึ่งเป็นเรื่องของ config ไม่ใช่ความพร้อมของ process
func (r *Registry) AllRunning() (bool, map[string]State) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := map[string]State{}
	ok := true
	for id, f := range r.flows {
		st := r.state[id]
		out[f.Spec().Name] = st
		if st != StateRunning && st != StateDegraded {
			ok = false
		}
	}
	return ok, out
}
