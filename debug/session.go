package debug

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/cuixudong74/debug-adapter-go/vm"
)

// Session states.
const (
	StateAttached   = "attached"
	StateDetached   = "detached"
	StateTerminated = "terminated"
)

// ErrSessionClosed is returned by operations on a detached/terminated session.
var ErrSessionClosed = errors.New("debug: session is closed")

// ErrTaskNotPaused is returned when inspecting a task this session has
// not paused; inspection is only defined at a deterministic stop point.
var ErrTaskNotPaused = errors.New("debug: task is not paused for this session")

// ErrPermissionDenied marks rejected reads of sensitive variables.
var ErrPermissionDenied = errors.New("debug: permission denied")

// frameRef resolves a session-scoped frame ID to a task frame.
type frameRef struct {
	taskID int
	index  int
}

// Session is one client's debug connection to a target. All mutable
// fields are guarded by the owning service's mutex.
type Session struct {
	id   int64
	svc  *Service
	ts   *targetState
	role string

	exceptionBreak bool
	bps            *breakpointStore
	steps          map[int]*stepState // taskID -> in-flight step
	events         *eventQueue

	frames    map[int]frameRef
	nextFrame int

	state string

	// pausesHeld counts active pauses this session must resume; used
	// to release them all on detach/terminate.
	pausesHeld map[int]bool // taskID -> held
}

// ID returns the session identifier.
func (s *Session) ID() int64 { return s.id }

// State returns the lifecycle state.
func (s *Session) State() string {
	s.svc.mu.Lock()
	defer s.svc.mu.Unlock()
	return s.state
}

// Role returns the session role ("admin" or "viewer").
func (s *Session) Role() string { return s.role }

// SetBreakpoints replaces the session's breakpoint set. The operation
// is idempotent: identical repeated calls keep IDs and never create
// duplicate stops. The set is also written to the target profile so a
// later attach restores it.
func (s *Session) SetBreakpoints(bps []Breakpoint) ([]Breakpoint, error) {
	s.svc.mu.Lock()
	defer s.svc.mu.Unlock()
	if err := s.checkOpenLocked(); err != nil {
		return nil, err
	}
	out := s.bps.set(bps)
	// Persist into the target profile (restore-on-attach policy).
	s.ts.profile.set(bps)
	return out, nil
}

// RemoveBreakpoint deletes a breakpoint by ID (idempotent).
func (s *Session) RemoveBreakpoint(id int) {
	s.svc.mu.Lock()
	defer s.svc.mu.Unlock()
	if s.state != StateAttached {
		return
	}
	s.bps.remove(id)
	s.ts.profile.remove(id)
}

// Breakpoints returns the session's current breakpoints.
func (s *Session) Breakpoints() []Breakpoint {
	s.svc.mu.Lock()
	defer s.svc.mu.Unlock()
	return s.bps.list()
}

// Continue resumes a paused task. It is a no-op if this session does
// not hold a pause for the task.
func (s *Session) Continue(taskID int) error {
	s.svc.mu.Lock()
	defer s.svc.mu.Unlock()
	if err := s.checkOpenLocked(); err != nil {
		return err
	}
	delete(s.steps, taskID)
	s.releasePauseLocked(taskID)
	return nil
}

// StepInto resumes a paused task for exactly one statement, crossing
// into callees. Only the stepped task is affected.
func (s *Session) StepInto(taskID int) error {
	return s.step(taskID, stepInto)
}

// StepOver resumes a paused task until the next statement at the same
// or shallower call depth.
func (s *Session) StepOver(taskID int) error {
	return s.step(taskID, stepOver)
}

func (s *Session) step(taskID int, mode stepMode) error {
	s.svc.mu.Lock()
	defer s.svc.mu.Unlock()
	if err := s.checkOpenLocked(); err != nil {
		return err
	}
	p, ok := s.ts.pauses[taskID]
	if !ok || !p.remaining[s.id] {
		return ErrTaskNotPaused
	}
	s.steps[taskID] = &stepState{mode: mode, depth: p.task.Depth()}
	s.releasePauseLocked(taskID)
	return nil
}

// releasePauseLocked lifts this session's hold on a task pause; the
// task resumes once every stopping session has released it.
func (s *Session) releasePauseLocked(taskID int) {
	p, ok := s.ts.pauses[taskID]
	if !ok || !p.remaining[s.id] {
		return
	}
	delete(p.remaining, s.id)
	delete(s.pausesHeld, taskID)
	s.events.emit(EventContinued, taskID, "", "")
	if len(p.remaining) == 0 {
		close(p.done)
	}
}

// StackTrace returns the call stack of a task paused for this session.
func (s *Session) StackTrace(taskID int) ([]StackFrame, error) {
	s.svc.mu.Lock()
	defer s.svc.mu.Unlock()
	if err := s.checkOpenLocked(); err != nil {
		return nil, err
	}
	if _, err := s.mustHoldPauseLocked(taskID); err != nil {
		return nil, err
	}
	out := make([]StackFrame, 0, len(s.frames))
	for id, ref := range s.frames {
		if ref.taskID != taskID {
			continue
		}
		t := s.ts.v.TaskByID(ref.taskID)
		f := t.Frames[ref.index]
		line := 0
		if f.PC >= 0 && f.PC < len(f.Fn.Code) {
			line = f.Fn.Code[f.PC].Line
		}
		out = append(out, StackFrame{ID: id, TaskID: taskID, Function: f.Fn.Name, Line: line})
	}
	// Deterministic order: deepest frame first.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if s.frames[out[j].ID].index > s.frames[out[i].ID].index {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

// Variables lists the variables of a frame, or the children of a
// nested value addressed by path. Sensitive values are redacted for
// non-admin sessions; explicitly addressing a sensitive variable is
// denied and audited.
func (s *Session) Variables(frameID int, path []string) ([]Variable, error) {
	s.svc.mu.Lock()
	defer s.svc.mu.Unlock()
	if err := s.checkOpenLocked(); err != nil {
		return nil, err
	}
	f, err := s.frameLocked(frameID)
	if err != nil {
		return nil, err
	}
	if len(path) == 0 {
		out := make([]Variable, 0, len(f.Locals))
		for name, v := range f.Locals {
			vb := makeVariable(name, v)
			if vb.Sensitive && s.role != "admin" {
				vb.Value = "<restricted>"
				vb.HasChildren = false
			}
			out = append(out, vb)
		}
		// Deterministic order by name.
		for i := 0; i < len(out); i++ {
			for j := i + 1; j < len(out); j++ {
				if out[j].Name < out[i].Name {
					out[i], out[j] = out[j], out[i]
				}
			}
		}
		return out, nil
	}
	// Resolve a path: first segment must be a local.
	root, ok := f.Locals[path[0]]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUndefinedSymbol, path[0])
	}
	for i, seg := range path {
		if isSensitiveName(seg) && s.role != "admin" {
			s.svc.addAuditLocked(s.id, "read-variable", joinPath(path[:i+1]), false)
			return nil, fmt.Errorf("%w: %q is sensitive", ErrPermissionDenied, seg)
		}
		if i == 0 {
			continue
		}
		if root, err = indexValue(root, seg); err != nil {
			return nil, err
		}
	}
	s.svc.addAuditLocked(s.id, "read-variable", joinPath(path), true)
	return childrenOf(root), nil
}

// Evaluate evaluates expr in the given frame without side effects.
// Cancellation/timeout aborts the evaluation and leaves the session
// state untouched.
func (s *Session) Evaluate(ctx context.Context, frameID int, expr string) (any, error) {
	s.svc.mu.Lock()
	if err := s.checkOpenLocked(); err != nil {
		s.svc.mu.Unlock()
		return nil, err
	}
	f, err := s.frameLocked(frameID)
	role := s.role
	s.svc.mu.Unlock()
	if err != nil {
		return nil, err
	}
	// Permission: any sensitive identifier requires the admin role.
	if role != "admin" {
		for _, id := range identsOf(expr) {
			if isSensitiveName(id) {
				s.svc.mu.Lock()
				s.svc.addAuditLocked(s.id, "evaluate", expr, false)
				s.svc.mu.Unlock()
				return nil, fmt.Errorf("%w: %q is sensitive", ErrPermissionDenied, id)
			}
		}
	}
	// Copy locals so evaluation cannot observe a resumed task's
	// mutations and cannot mutate program state.
	scope := make(map[string]any, len(f.Locals))
	for k, v := range f.Locals {
		scope[k] = v
	}
	v, err := evalExpr(ctx, scope, expr)
	if err != nil {
		return nil, err
	}
	s.svc.mu.Lock()
	s.svc.addAuditLocked(s.id, "evaluate", expr, true)
	s.svc.mu.Unlock()
	return v, nil
}

// TaskState reports whether a task is running, paused, or terminated,
// so clients can distinguish a crash-paused task from a finished one.
func (s *Session) TaskState(taskID int) (string, error) {
	s.svc.mu.Lock()
	defer s.svc.mu.Unlock()
	if err := s.checkOpenLocked(); err != nil {
		return "", err
	}
	if _, ok := s.ts.pauses[taskID]; ok {
		return "paused", nil
	}
	t := s.ts.v.TaskByID(taskID)
	if t == nil {
		return "", fmt.Errorf("debug: unknown task %d", taskID)
	}
	if t.Done() {
		return "terminated", nil
	}
	return "running", nil
}

// Events returns the session's bounded event channel. It is closed
// when the session detaches or terminates; no events arrive after.
func (s *Session) Events() <-chan Event { return s.events.C() }

// Detach detaches the session. The target keeps running at its own
// pace: every pause held by this session is released and the session
// receives no further events. Its breakpoints persist in the target
// profile for the next attach.
func (s *Session) Detach() { s.close(StateDetached) }

// Terminate ends the session; no further events are delivered.
func (s *Session) Terminate() { s.close(StateTerminated) }

func (s *Session) close(state string) {
	s.svc.mu.Lock()
	defer s.svc.mu.Unlock()
	if s.state != StateAttached {
		return // idempotent
	}
	s.state = state
	for taskID := range s.pausesHeld {
		s.releasePauseLocked(taskID)
	}
	delete(s.ts.sessions, s.id)
	s.events.close()
}

// --- internal helpers (called with svc.mu held unless noted) ---

func (s *Session) checkOpenLocked() error {
	if s.state != StateAttached {
		return ErrSessionClosed
	}
	return nil
}

func (s *Session) mustHoldPauseLocked(taskID int) (*pause, error) {
	p, ok := s.ts.pauses[taskID]
	if !ok || !p.remaining[s.id] {
		return nil, ErrTaskNotPaused
	}
	return p, nil
}

func (s *Session) frameLocked(frameID int) (*vm.Frame, error) {
	ref, ok := s.frames[frameID]
	if !ok {
		return nil, fmt.Errorf("debug: unknown frame %d", frameID)
	}
	if _, err := s.mustHoldPauseLocked(ref.taskID); err != nil {
		return nil, err
	}
	t := s.ts.v.TaskByID(ref.taskID)
	if t == nil || ref.index >= len(t.Frames) {
		return nil, fmt.Errorf("debug: stale frame %d", frameID)
	}
	return t.Frames[ref.index], nil
}

// snapshotFramesLocked records frame IDs for a freshly paused task.
func (s *Session) snapshotFramesLocked(t *vm.Task) {
	for id, ref := range s.frames {
		if ref.taskID == t.ID {
			delete(s.frames, id)
		}
	}
	for i := range t.Frames {
		s.frames[s.nextFrame] = frameRef{taskID: t.ID, index: i}
		s.nextFrame++
	}
}

// shouldStopLocked decides whether this session wants to stop the task
// at its current instruction, and the reason. Deterministic: step
// state is checked first, then an exact breakpoint lookup, then the
// optional condition (pure evaluation; errors mean "no hit").
func (s *Session) shouldStopLocked(t *vm.Task) (StopReason, bool) {
	// Stops only happen at statement boundaries so one source line
	// yields exactly one stop, never duplicate pauses.
	if !t.AtLineStart() {
		return "", false
	}
	if st, ok := s.steps[t.ID]; ok {
		hit := false
		switch st.mode {
		case stepInto:
			hit = true // next statement, any depth
		case stepOver:
			hit = t.Depth() <= st.depth
		}
		if hit {
			delete(s.steps, t.ID)
			return ReasonStep, true
		}
		return "", false
	}
	bp := s.bps.match(t.CurrentFunction(), t.CurrentLine())
	if bp == nil {
		return "", false
	}
	if bp.Condition == "" {
		return ReasonBreakpoint, true
	}
	top := t.Frames[len(t.Frames)-1]
	v, err := evalExpr(context.Background(), top.Locals, bp.Condition)
	if err != nil {
		return "", false // deterministic: bad condition never hits
	}
	if b, ok := v.(bool); ok && b {
		return ReasonBreakpoint, true
	}
	return "", false
}

var identRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// identsOf extracts identifiers from an expression for permission
// checks. It never fails; unknown syntax simply yields no identifiers.
func identsOf(expr string) []string { return identRe.FindAllString(expr, -1) }

func joinPath(p []string) string {
	out := ""
	for i, s := range p {
		if i > 0 {
			out += "."
		}
		out += s
	}
	return out
}
