package debug

import (
	"fmt"
	"sync"
)

// breakpointStore is an idempotent breakpoint set keyed by
// (function, line). Re-setting an existing breakpoint keeps its ID and
// updates the condition; removing a missing ID is a no-op. Hit
// evaluation is a pure map lookup plus condition check, so it is
// deterministic and cannot produce duplicate pauses for one location.
type breakpointStore struct {
	mu     sync.Mutex
	byKey  map[string]*Breakpoint
	byID   map[int]*Breakpoint
	nextID int
}

func newBreakpointStore() *breakpointStore {
	return &breakpointStore{
		byKey:  make(map[string]*Breakpoint),
		byID:   make(map[int]*Breakpoint),
		nextID: 1,
	}
}

func bpKey(function string, line int) string {
	return fmt.Sprintf("%s:%d", function, line)
}

// set replaces the whole set. Breakpoints whose (function, line)
// already existed keep their IDs, making repeated calls idempotent.
func (s *breakpointStore) set(bps []Breakpoint) []Breakpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Breakpoint, 0, len(bps))
	seen := make(map[string]bool, len(bps))
	keep := make(map[int]bool, len(bps))
	for _, bp := range bps {
		key := bpKey(bp.Function, bp.Line)
		if seen[key] {
			continue // duplicate in request: ignore
		}
		seen[key] = true
		if old, ok := s.byKey[key]; ok {
			old.Condition = bp.Condition
			keep[old.ID] = true
			out = append(out, *old)
			continue
		}
		nb := &Breakpoint{ID: s.nextID, Function: bp.Function, Line: bp.Line, Condition: bp.Condition}
		s.nextID++
		s.byKey[key] = nb
		s.byID[nb.ID] = nb
		keep[nb.ID] = true
		out = append(out, *nb)
	}
	// Drop breakpoints not present in the new set.
	for id := range s.byID {
		if !keep[id] {
			bp := s.byID[id]
			delete(s.byID, id)
			delete(s.byKey, bpKey(bp.Function, bp.Line))
		}
	}
	return out
}

// add upserts a single breakpoint (idempotent per location).
func (s *breakpointStore) add(bp Breakpoint) Breakpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := bpKey(bp.Function, bp.Line)
	if old, ok := s.byKey[key]; ok {
		old.Condition = bp.Condition
		return *old
	}
	nb := &Breakpoint{ID: s.nextID, Function: bp.Function, Line: bp.Line, Condition: bp.Condition}
	s.nextID++
	s.byKey[key] = nb
	s.byID[nb.ID] = nb
	return *nb
}

// remove deletes by ID; missing IDs are a no-op (idempotent).
func (s *breakpointStore) remove(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bp, ok := s.byID[id]; ok {
		delete(s.byID, id)
		delete(s.byKey, bpKey(bp.Function, bp.Line))
	}
}

// list returns a stable copy of the current set.
func (s *breakpointStore) list() []Breakpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Breakpoint, 0, len(s.byID))
	for _, bp := range s.byID {
		out = append(out, *bp)
	}
	return out
}

// match returns the breakpoint at (function, line), or nil.
func (s *breakpointStore) match(function string, line int) *Breakpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bp, ok := s.byKey[bpKey(function, line)]; ok {
		cp := *bp
		return &cp
	}
	return nil
}

// clone copies all breakpoints into the receiver (used to restore the
// persisted profile on attach).
func (s *breakpointStore) cloneFrom(src *breakpointStore) {
	for _, bp := range src.list() {
		s.add(bp)
	}
}
