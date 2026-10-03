package debug

import "sync"

// Breakpoint is a source line breakpoint.
type Breakpoint struct {
	ID        int    `json:"id"`
	File      string `json:"file"`
	Line      int    `json:"line"`
	Condition string `json:"condition,omitempty"`
	Verified  bool   `json:"verified"`
	HitCount  int    `json:"hitCount"`
}

// ExceptionFilter controls when the debugger stops on exceptions.
type ExceptionFilter string

const (
	ExNone     ExceptionFilter = "none"
	ExUncaught ExceptionFilter = "uncaught"
	ExAll      ExceptionFilter = "all"
)

type loc struct {
	file string
	line int
}

// breakpointRegistry is the target-level breakpoint store. It persists across
// detach/attach and is removed only when the target terminates.
type breakpointRegistry struct {
	mu     sync.Mutex
	bps    map[int]*Breakpoint
	byLoc  map[loc]*Breakpoint
	nextID int
}

func newBreakpointRegistry() *breakpointRegistry {
	return &breakpointRegistry{
		bps:   map[int]*Breakpoint{},
		byLoc: map[loc]*Breakpoint{},
	}
}

// Set adds a breakpoint. Setting a breakpoint at a location that already has
// one is idempotent: the existing breakpoint is returned.
func (r *breakpointRegistry) Set(file string, line int, cond string) *Breakpoint {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := loc{file, line}
	if existing, ok := r.byLoc[k]; ok {
		return existing
	}
	r.nextID++
	bp := &Breakpoint{
		ID:        r.nextID,
		File:      file,
		Line:      line,
		Condition: cond,
		Verified:  true,
	}
	r.bps[bp.ID] = bp
	r.byLoc[k] = bp
	return bp
}

// Delete removes a breakpoint by id. Deleting a missing breakpoint is a no-op.
func (r *breakpointRegistry) Delete(id int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	bp, ok := r.bps[id]
	if !ok {
		return false
	}
	delete(r.bps, id)
	delete(r.byLoc, loc{bp.File, bp.Line})
	return true
}

// DeleteByLocation removes the breakpoint at the given location.
func (r *breakpointRegistry) DeleteByLocation(file string, line int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	bp, ok := r.byLoc[loc{file, line}]
	if !ok {
		return false
	}
	delete(r.bps, bp.ID)
	delete(r.byLoc, loc{file, line})
	return true
}

// At returns the breakpoint at the given location, if any.
func (r *breakpointRegistry) At(file string, line int) (*Breakpoint, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	bp, ok := r.byLoc[loc{file, line}]
	return bp, ok
}

// Get returns a breakpoint by id.
func (r *breakpointRegistry) Get(id int) (*Breakpoint, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	bp, ok := r.bps[id]
	return bp, ok
}

// List returns all breakpoints in id order.
func (r *breakpointRegistry) List() []*Breakpoint {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Breakpoint, 0, len(r.bps))
	for _, bp := range r.bps {
		out = append(out, bp)
	}
	sortBreakpoints(out)
	return out
}

// Hit records a hit and returns the new count.
func (r *breakpointRegistry) Hit(id int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if bp, ok := r.bps[id]; ok {
		bp.HitCount++
		return bp.HitCount
	}
	return 0
}

func sortBreakpoints(bps []*Breakpoint) {
	for i := 1; i < len(bps); i++ {
		for j := i; j > 0 && bps[j-1].ID > bps[j].ID; j-- {
			bps[j-1], bps[j] = bps[j], bps[j-1]
		}
	}
}
