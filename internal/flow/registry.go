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
