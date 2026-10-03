package debug

import (
	"context"
	"fmt"
	"sync"

	"github.com/cuixudong74/debug-adapter-go/runtime"
)

// Session states.
const (
	stateRunning    = "running"
	statePaused     = "paused"
	stateTerminated = "terminated"
	stateDetached   = "detached"
)

// stepMode identifies a stepping operation.
type stepMode int

const (
	stepIn stepMode = iota
	stepOver
	stepOut
)

type stepState struct {
	mode  stepMode
	depth int
}

// Target is a running script target with its breakpoint set.
type Target struct {
	id   string
	name string
	rt   *runtime.Target

	mu       sync.Mutex
	session  *Session
	breakpts *breakpointRegistry
}

// ID returns the target id.
func (t *Target) ID() string { return t.id }

// Name returns the target name.
func (t *Target) Name() string { return t.name }

// Session is a debug session attached to one target. It implements
// runtime.Hook.
type Session struct {
	id       string
	targetID string
	target   *Target
	user     User
	svc      *Service

	mu       sync.Mutex
	state    string
	steps    map[int]*stepState
	exFilter ExceptionFilter

	refs      *refTable
	frames    map[int]*runtime.Frame // frame id -> frame, valid while paused
	frameRefs map[int]int            // frame id -> locals variablesReference
	nextFrameID int

	lastException *runtime.Value

	events *EventQueue
}

// ID returns the session id.
func (s *Session) ID() string { return s.id }

// TargetID returns the attached target id.
func (s *Session) TargetID() string { return s.targetID }

// State returns the session state: running, paused, terminated, or detached.
func (s *Session) State() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// User returns the session user.
func (s *Session) User() User { return s.user }

// Tasks returns the tasks of the attached target.
func (s *Session) Tasks() []*runtime.Task {
	return s.target.rt.Tasks()
}

// FrameRefs returns a snapshot of frame id -> locals variablesReference, valid
// while paused.
func (s *Session) FrameRefs() map[int]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int]int, len(s.frameRefs))
	for id, ref := range s.frameRefs {
		out[id] = ref
	}
	return out
}

// Events returns the session's event queue.
func (s *Session) Events() *EventQueue { return s.events }

// ---- runtime.Hook implementation ----

// BeforeLine is called by the runtime before each source line executes.
func (s *Session) BeforeLine(t *runtime.Task) {
	s.mu.Lock()
	ended := s.state == stateTerminated || s.state == stateDetached
	s.mu.Unlock()
	if ended {
		return
	}

	f := t.Top()
	file, line := f.File, f.Line

	// 1. breakpoints (including conditional)
	if bp, ok := s.target.breakpts.At(file, line); ok {
		hit := true
		if bp.Condition != "" {
			v, err := s.evalCondition(bp.Condition, f)
			if err != nil {
				s.events.Emit(&Event{
					Kind: EvDiagnostic,
					Message: fmt.Sprintf("breakpoint %d condition evaluation failed: %v", bp.ID, err),
				})
				hit = false
			} else {
				hit = v.IsTruthy()
			}
		}
		if hit {
			s.target.breakpts.Hit(bp.ID)
			s.stopTask(t, ReasonBreakpoint, bp.ID)
			return
		}
	}

	// 2. stepping
	s.mu.Lock()
	step, ok := s.steps[t.ID]
	s.mu.Unlock()
	if ok {
		depth := t.Depth()
		hit := false
		switch step.mode {
		case stepIn:
			hit = true
		case stepOver:
			hit = depth <= step.depth
		case stepOut:
			hit = depth < step.depth
		}
		if hit {
			s.stopTask(t, ReasonStep, 0)
		}
	}
}

// OnThrow is called when a task raises a script exception.
func (s *Session) OnThrow(t *runtime.Task, thrown *runtime.Value) bool {
	s.mu.Lock()
	ended := s.state == stateTerminated || s.state == stateDetached
	filter := s.exFilter
	s.mu.Unlock()
	if ended {
		return false
	}
	if filter != ExUncaught && filter != ExAll {
		return false
	}
	s.mu.Lock()
	s.lastException = thrown
	if s.state == stateRunning {
		s.state = statePaused
	}
	s.mu.Unlock()
	s.events.Emit(&Event{Kind: EvStopped, TaskID: t.ID, Reason: ReasonException})
	t.SetPaused(true)
	t.WaitWhilePaused()
	s.mu.Lock()
	delete(s.steps, t.ID)
	s.mu.Unlock()
	return true
}

// OnTaskDone is called when a task finishes.
func (s *Session) OnTaskDone(t *runtime.Task, thrown *runtime.Value, fatal error) {
	s.mu.Lock()
	ended := s.state == stateTerminated || s.state == stateDetached
	s.mu.Unlock()
	if ended {
		return
	}
	if t != s.target.rt.Main() {
		s.events.Emit(&Event{Kind: EvThread, TaskID: t.ID, ThreadStatus: "exited"})
		return
	}
	// main task ended -> target terminated
	s.mu.Lock()
	s.state = stateTerminated
	if thrown != nil {
		s.lastException = thrown
	}
	s.mu.Unlock()
	s.refs.clear()
	evt := &Event{Kind: EvTerminated}
	if fatal != nil {
		evt.Fatal = true
		evt.Exception = fatal.Error()
	} else if thrown != nil {
		evt.Exception = thrown.Display()
	}
	s.events.Emit(evt)
	s.events.Close()
}

// ---- control ----

// Continue resumes all paused tasks.
func (s *Session) Continue(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return newError(KindCanceled, "continue canceled: "+err.Error())
	}
	s.mu.Lock()
	if s.state != statePaused {
		st := s.state
		s.mu.Unlock()
		return errorf(KindInvalidState, "cannot continue: session is %s", st)
	}
	s.state = stateRunning
	s.steps = map[int]*stepState{}
	s.mu.Unlock()

	s.invalidateRefs()
	s.target.rt.ResumeAll()
	s.events.Emit(&Event{Kind: EvContinued})
	return nil
}

// Step arms a step for the given task and resumes it. Only that task is
// resumed; other paused tasks stay paused.
func (s *Session) Step(ctx context.Context, taskID int, mode stepMode) error {
	if err := ctx.Err(); err != nil {
		return newError(KindCanceled, "step canceled: "+err.Error())
	}
	s.mu.Lock()
	if s.state != statePaused {
		st := s.state
		s.mu.Unlock()
		return errorf(KindInvalidState, "cannot step: session is %s", st)
	}
	tk, ok := s.target.rt.TaskByID(taskID)
	if !ok {
		s.mu.Unlock()
		return errorf(KindNotFound, "task %d not found", taskID)
	}
	if !tk.IsPaused() {
		s.mu.Unlock()
		return errorf(KindInvalidState, "task %d is not paused", taskID)
	}
	depth := tk.Depth()
	s.steps[taskID] = &stepState{mode: mode, depth: depth}
	s.state = stateRunning
	s.mu.Unlock()

	s.invalidateRefs()
	tk.SetPaused(false)
	s.events.Emit(&Event{Kind: EvContinued, TaskID: taskID})
	return nil
}

// StepIn steps into the next call.
func (s *Session) StepIn(ctx context.Context, taskID int) error {
	return s.Step(ctx, taskID, stepIn)
}

// StepOver steps over the current line.
func (s *Session) StepOver(ctx context.Context, taskID int) error {
	return s.Step(ctx, taskID, stepOver)
}

// StepOut steps out of the current function.
func (s *Session) StepOut(ctx context.Context, taskID int) error {
	return s.Step(ctx, taskID, stepOut)
}

// Pause requests a pause of a running task.
func (s *Session) Pause(ctx context.Context, taskID int) error {
	if err := ctx.Err(); err != nil {
		return newError(KindCanceled, "pause canceled: "+err.Error())
	}
	s.mu.Lock()
	if s.state == stateTerminated || s.state == stateDetached {
		s.mu.Unlock()
		return errorf(KindInvalidState, "session %s", s.state)
	}
	tk, ok := s.target.rt.TaskByID(taskID)
	if !ok {
		s.mu.Unlock()
		return errorf(KindNotFound, "task %d not found", taskID)
	}
	s.mu.Unlock()
	tk.SetPaused(true)
	s.mu.Lock()
	if s.state == stateRunning {
		s.state = statePaused
	}
	s.mu.Unlock()
	s.events.Emit(&Event{Kind: EvStopped, TaskID: taskID, Reason: ReasonPause})
	return nil
}

// Detach ends the session but leaves the target running at its own pace.
// Breakpoints are retained and restored on re-attach.
func (s *Session) Detach(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return newError(KindCanceled, "detach canceled: "+err.Error())
	}
	s.mu.Lock()
	if s.state == stateTerminated || s.state == stateDetached {
		s.mu.Unlock()
		return errorf(KindInvalidState, "session already %s", s.state)
	}
	s.state = stateDetached
	s.mu.Unlock()

	s.target.mu.Lock()
	s.target.session = nil
	s.target.mu.Unlock()
	s.target.rt.SetHook(nil)
	s.target.rt.ResumeAll()
	s.events.Close()
	s.svc.detachSession(s)
	return nil
}

// Terminate ends the target and the session.
func (s *Session) Terminate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return newError(KindCanceled, "terminate canceled: "+err.Error())
	}
	s.mu.Lock()
	if s.state == stateTerminated {
		s.mu.Unlock()
		return errorf(KindInvalidState, "session already terminated")
	}
	s.state = stateTerminated
	s.mu.Unlock()

	s.target.mu.Lock()
	s.target.session = nil
	s.target.mu.Unlock()
	s.target.rt.Shutdown()
	s.events.Emit(&Event{Kind: EvTerminated, Fatal: false})
	s.events.Close()
	s.svc.finishSession(s)
	return nil
}

// ---- breakpoints ----

// SetBreakpoints idempotently sets breakpoints for a file.
func (s *Session) SetBreakpoints(ctx context.Context, file string, bps []BreakpointSpec) ([]*Breakpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, newError(KindCanceled, "setBreakpoints canceled: "+err.Error())
	}
	s.mu.Lock()
	if s.state == stateTerminated || s.state == stateDetached {
		s.mu.Unlock()
		return nil, errorf(KindInvalidState, "session %s", s.state)
	}
	s.mu.Unlock()
	out := make([]*Breakpoint, 0, len(bps))
	for _, spec := range bps {
		bp := s.target.breakpts.Set(file, spec.Line, spec.Condition)
		out = append(out, bp)
	}
	s.audit(AuditEntry{
		User: s.user.ID, Action: "breakpoint:set", SessionID: s.id,
		TargetID: s.targetID, Granted: true,
		Reason: fmt.Sprintf("file=%s count=%d", file, len(bps)),
	})
	return out, nil
}

// BreakpointSpec is a requested breakpoint.
type BreakpointSpec struct {
	Line      int    `json:"line"`
	Condition string `json:"condition,omitempty"`
}

// DeleteBreakpoints removes breakpoints by id. Missing ids are ignored.
func (s *Session) DeleteBreakpoints(ctx context.Context, ids []int) error {
	if err := ctx.Err(); err != nil {
		return newError(KindCanceled, "deleteBreakpoints canceled: "+err.Error())
	}
	for _, id := range ids {
		s.target.breakpts.Delete(id)
	}
	return nil
}

// Breakpoints returns all breakpoints of the attached target.
func (s *Session) Breakpoints(ctx context.Context) ([]*Breakpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, newError(KindCanceled, "breakpoints canceled: "+err.Error())
	}
	return s.target.breakpts.List(), nil
}

// SetExceptionFilter configures when the debugger stops on exceptions.
func (s *Session) SetExceptionFilter(ctx context.Context, filter ExceptionFilter) error {
	if err := ctx.Err(); err != nil {
		return newError(KindCanceled, "setExceptionFilter canceled: "+err.Error())
	}
	switch filter {
	case ExNone, ExUncaught, ExAll:
	default:
		return errorf(KindInvalidArg, "unknown exception filter %q", filter)
	}
	s.mu.Lock()
	s.exFilter = filter
	s.mu.Unlock()
	return nil
}

// ---- inspection ----

// FrameInfo describes a stack frame.
type FrameInfo struct {
	ID             int    `json:"id"`
	Name           string `json:"name"`
	File           string `json:"file"`
	Line           int    `json:"line"`
	LocalsReference int   `json:"localsReference"`
}

// Stack returns the call stack of a task, top frame first.
func (s *Session) Stack(ctx context.Context, taskID int) ([]FrameInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, newError(KindCanceled, "stack canceled: "+err.Error())
	}
	s.mu.Lock()
	if s.state != statePaused {
		s.mu.Unlock()
		return nil, newError(KindInvalidState, "stack only available while paused")
	}
	s.mu.Unlock()
	tk, ok := s.target.rt.TaskByID(taskID)
	if !ok {
		return nil, errorf(KindNotFound, "task %d not found", taskID)
	}
	frames := tk.Frames()
	out := make([]FrameInfo, 0, len(frames))
	s.mu.Lock()
	s.frames = map[int]*runtime.Frame{}
	s.frameRefs = map[int]int{}
	s.nextFrameID = 0
	for i := len(frames) - 1; i >= 0; i-- {
		f := frames[i]
		id := s.nextFrameID
		s.nextFrameID++
		s.frames[id] = f
		ref := s.refs.put(&refEntry{kind: refLocals, frame: f})
		s.frameRefs[id] = ref
		out = append(out, FrameInfo{
			ID: id, Name: f.Func.Name, File: f.File, Line: f.Line,
			LocalsReference: ref,
		})
	}
	s.mu.Unlock()
	return out, nil
}

// Evaluate evaluates an expression in the context of a frame. Evaluation is
// side-effect free and never changes program state.
func (s *Session) Evaluate(ctx context.Context, expr string, frameID int) (*Variable, error) {
	if err := ctx.Err(); err != nil {
		return nil, newError(KindCanceled, "evaluate canceled: "+err.Error())
	}
	if !s.user.HasScope(ScopeEvaluate) {
		s.audit(AuditEntry{
			User: s.user.ID, Action: "evaluate", SessionID: s.id,
			TargetID: s.targetID, Variable: expr, Granted: false,
			Reason: "missing scope evaluate",
		})
		return nil, newError(KindPermission, "evaluate denied: missing scope evaluate")
	}
	s.mu.Lock()
	if s.state != statePaused {
		s.mu.Unlock()
		return nil, newError(KindInvalidState, "evaluate only available while paused")
	}
	frame, ok := s.frames[frameID]
	s.mu.Unlock()
	if !ok {
		return nil, errorf(KindNotFound, "frame %d not found", frameID)
	}

	// Permission: referencing sensitive names requires elevated scope.
	if sensitiveReads(expr) && !s.user.HasScope(ScopeReadSensitive) {
		s.audit(AuditEntry{
			User: s.user.ID, Action: "evaluate", SessionID: s.id,
			TargetID: s.targetID, Variable: expr, Granted: false,
			Reason: "references sensitive name without scope variables:read_sensitive",
		})
		return nil, newError(KindPermission, "evaluate denied: expression references sensitive variable")
	}

	scope := mapScope{maps: []map[string]*runtime.Value{
		frame.Locals,
		s.target.rt.Program().Globals(),
	}}
	v, err := EvalExpression(expr, scope)
	if err != nil {
		s.audit(AuditEntry{
			User: s.user.ID, Action: "evaluate", SessionID: s.id,
			TargetID: s.targetID, Variable: expr, Granted: true,
			Reason: "eval error: " + err.Error(),
		})
		return nil, err
	}
	s.audit(AuditEntry{
		User: s.user.ID, Action: "evaluate", SessionID: s.id,
		TargetID: s.targetID, Variable: expr, Granted: true,
	})
	out := s.makeVar("result", v, false)
	return &out, nil
}

// LastException returns the exception captured at an exception stop, if any.
func (s *Session) LastException(ctx context.Context) (*Variable, error) {
	if err := ctx.Err(); err != nil {
		return nil, newError(KindCanceled, "exception lookup canceled: "+err.Error())
	}
	s.mu.Lock()
	if s.state != statePaused && s.state != stateTerminated {
		s.mu.Unlock()
		return nil, newError(KindInvalidState, "no exception in state "+s.state)
	}
	ex := s.lastException
	s.mu.Unlock()
	if ex == nil {
		return nil, newError(KindNotFound, "no exception captured")
	}
	out := s.makeVar("exception", ex, false)
	return &out, nil
}

// ---- internals ----

func (s *Session) stopTask(t *runtime.Task, reason string, bpID int) {
	t.SetPaused(true)
	s.mu.Lock()
	if s.state == stateRunning {
		s.state = statePaused
	}
	// clear any consumed step before waiting so that a step armed by the
	// client while we are paused is not deleted when we wake.
	delete(s.steps, t.ID)
	s.mu.Unlock()
	s.events.Emit(&Event{
		Kind: EvStopped, TaskID: t.ID, Reason: reason, BreakpointID: bpID,
	})
	t.WaitWhilePaused()
}

func (s *Session) evalCondition(expr string, f *runtime.Frame) (*runtime.Value, error) {
	scope := mapScope{maps: []map[string]*runtime.Value{
		f.Locals,
		s.target.rt.Program().Globals(),
	}}
	return EvalExpression(expr, scope)
}

func (s *Session) invalidateRefs() {
	s.refs.clear()
	s.mu.Lock()
	s.frames = map[int]*runtime.Frame{}
	s.frameRefs = map[int]int{}
	s.nextFrameID = 0
	s.mu.Unlock()
}

func (s *Session) audit(e AuditEntry) {
	e.SessionID = s.id
	e.TargetID = s.targetID
	e.User = s.user.ID
	s.svc.audit.Record(e)
}

// sensitiveReads reports whether an expression text mentions a sensitive name.
func sensitiveReads(expr string) bool {
	for _, sub := range sensitiveSubstrings {
		if containsWord(expr, sub) {
			return true
		}
	}
	return false
}

func containsWord(s, word string) bool {
	// case-insensitive substring match on identifier boundaries
	ls := toLower(s)
	lw := toLower(word)
	idx := 0
	for {
		i := indexOf(ls, lw, idx)
		if i < 0 {
			return false
		}
		before := i == 0 || !isIdentChar(ls[i-1])
		after := i+len(lw) >= len(ls) || !isIdentChar(ls[i+len(lw)])
		if before && after {
			return true
		}
		idx = i + 1
	}
}

func indexOf(s, sub string, start int) int {
	for i := start; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func isIdentChar(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func toLower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
