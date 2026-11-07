package store

import (
	"sort"
	"sync"
	"time"
)

// MemStore is an in-memory Store with the same behavior as FileStore. It
// exists so command-level tests can run against the Store interface without
// touching the filesystem; it is not used by the lease binary.
type MemStore struct {
	mu      sync.Mutex
	holders map[string]map[int]*Holder // resource -> slot -> holder
	waiters map[string][]Waiter        // resource -> tickets
	events  []Event
}

// NewMemStore returns an empty in-memory Store.
func NewMemStore() *MemStore {
	return &MemStore{
		holders: map[string]map[int]*Holder{},
		waiters: map[string][]Waiter{},
	}
}

func (m *MemStore) Holder(resource string, slot int) (*Holder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.holders[resource][slot]
	if !ok {
		return nil, nil
	}
	cp := *h
	return &cp, nil
}

func (m *MemStore) Holders() ([]Holder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Holder{}
	resources := make([]string, 0, len(m.holders))
	for resource := range m.holders {
		resources = append(resources, resource)
	}
	sort.Strings(resources)
	for _, resource := range resources {
		slots := m.holders[resource]
		nums := make([]int, 0, len(slots))
		for slot := range slots {
			nums = append(nums, slot)
		}
		sort.Ints(nums)
		for _, slot := range nums {
			out = append(out, *slots[slot])
		}
	}
	return out, nil
}

func (m *MemStore) SetHolder(h *Holder) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.holders[h.Resource] == nil {
		m.holders[h.Resource] = map[int]*Holder{}
	}
	cp := *h
	m.holders[h.Resource][h.Slot] = &cp
	return nil
}

func (m *MemStore) ClearHolder(resource string, slot int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.holders[resource], slot)
	return nil
}

func (m *MemStore) Waiters(resource string) ([]Waiter, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Waiter, 0, len(m.waiters[resource]))
	out = append(out, m.waiters[resource]...)
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out, nil
}

func (m *MemStore) EnqueueWaiter(w *Waiter) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *w
	m.waiters[w.Resource] = append(m.waiters[w.Resource], cp)
	return nil
}

func (m *MemStore) DropWaiter(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for resource, tickets := range m.waiters {
		kept := tickets[:0]
		for _, w := range tickets {
			if w.ID != id {
				kept = append(kept, w)
			}
		}
		m.waiters[resource] = kept
	}
	return nil
}

func (m *MemStore) AppendEvent(e Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	m.events = append(m.events, e)
	return nil
}

func (m *MemStore) Events() ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Event, len(m.events))
	copy(out, m.events)
	return out, nil
}

func (m *MemStore) Resources() ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	for resource := range m.holders {
		seen[resource] = true
	}
	for resource := range m.waiters {
		seen[resource] = true
	}
	for _, e := range m.events {
		if e.Resource != "" {
			seen[e.Resource] = true
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}
