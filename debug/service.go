package debug

import (
	"errors"
	"sync"

	"github.com/cuixudong74/debug-adapter-go/vm"
)

// ErrTargetNotFound is returned when attaching to an unknown target.
var ErrTargetNotFound = errors.New("debug: target not found")

const (
	eventQueueCap = 256
	auditCap      = 1024
)

// pause is an active stop of one task. The task's goroutine blocks on
// done until every session in remaining has released the pause, so a
// paused execution unit makes no progress while any holder inspects
// it, and no other execution unit is affected.
type pause struct {
	task      *vm.Task
	reason    StopReason
	remaining map[int64]bool // session IDs that must still resume
	done      chan struct{}
}

// targetState is the per-VM state shared by its sessions.
type targetState struct {
	name     string
	v        *vm.VM
	sessions map[int64]*Session
	profile  *breakpointStore // persisted breakpoint set (restore policy)
	pauses   map[int]*pause   // taskID -> active pause
}

// Service owns targets (VMs) and sessions. It implements vm.Hook and
// dispatches execution callbacks to the attached sessions.
type Service struct {
	mu       sync.Mutex
	targets  map[string]*targetState
	byVM     map[*vm.VM]*targetState
	nextSess int64

	audit     []AuditEntry
	nextAudit int64
}

// NewService creates an empty debug service.
func NewService() *Service {
	return &Service{
		targets:  make(map[string]*targetState),
		byVM:     make(map[*vm.VM]*targetState),
		nextSess: 1,
	}
}

// RegisterTarget makes a VM attachable under name. The service takes
// over as the VM's hook.
func (s *Service) RegisterTarget(name string, v *vm.VM) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := &targetState{
		name:     name,
		v:        v,
		sessions: make(map[int64]*Session),
		profile:  newBreakpointStore(),
		pauses:   make(map[int]*pause),
	}
	s.targets[name] = ts
	s.byVM[v] = ts
	v.SetHook(s)
}

// Attach creates a session on the named target. The session inherits
// the target's persisted breakpoint profile (restore-on-attach).
func (s *Service) Attach(req AttachRequest) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, ok := s.targets[req.Target]
	if !ok {
		return nil, ErrTargetNotFound
	}
	role := req.Role
	if role == "" {
		role = "viewer" // old clients omit the field: least privilege
	}
	exceptionBreak := true
	if req.ExceptionBreak != nil {
		exceptionBreak = *req.ExceptionBreak
	}
	sess := &Session{
		id:             s.nextSess,
		svc:            s,
		ts:             ts,
		role:           role,
		exceptionBreak: exceptionBreak,
		bps:            newBreakpointStore(),
		steps:          make(map[int]*stepState),
		events:         newEventQueue(eventQueueCap),
		frames:         make(map[int]frameRef),
		nextFrame:      1,
		state:          StateAttached,
		pausesHeld:     make(map[int]bool),
	}
	s.nextSess++
	sess.bps.cloneFrom(ts.profile)
	ts.sessions[sess.id] = sess
	return sess, nil
}

// Audit returns a copy of the bounded audit log (oldest entries are
// dropped once the cap is reached).
func (s *Service) Audit() []AuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AuditEntry, len(s.audit))
	copy(out, s.audit)
	return out
}

// addAuditLocked appends an audit entry, evicting the oldest when full.
func (s *Service) addAuditLocked(sessionID int64, action, detail string, allowed bool) {
	s.nextAudit++
	s.audit = append(s.audit, AuditEntry{
		Seq:       s.nextAudit,
		SessionID: sessionID,
		Action:    action,
		Detail:    detail,
		Allowed:   allowed,
	})
	if len(s.audit) > auditCap {
		s.audit = s.audit[len(s.audit)-auditCap:]
	}
}

// --- vm.Hook implementation ---

// OnStep runs on the task's goroutine before each instruction. If any
// attached session wants to stop, the task blocks until all stopping
// sessions resume it.
func (s *Service) OnStep(t *vm.Task) {
	s.mu.Lock()
	ts := s.byVM[t.VM()]
	if ts == nil {
		s.mu.Unlock()
		return
	}
	var stoppers []*Session
	reason := StopReason("")
	for _, sess := range ts.sessions {
		if sess.state != StateAttached {
			continue
		}
		if r, hit := sess.shouldStopLocked(t); hit {
			stoppers = append(stoppers, sess)
			if reason == "" {
				reason = r
			}
		}
	}
	if len(stoppers) == 0 {
		s.mu.Unlock()
		return
	}
	s.beginPauseLocked(ts, t, stoppers, reason)
	s.mu.Unlock()

	for _, sess := range stoppers {
		sess.events.emit(EventStopped, t.ID, reason, "")
	}
	<-p_done(ts, t) // block this task's goroutine until fully resumed
	s.finishPause(ts, t)
}

// OnException preserves the crash scene: frames stay intact and, if
// any attached session breaks on exceptions, the task pauses so the
// session can inspect it before the task terminates.
func (s *Service) OnException(t *vm.Task, err error) {
	s.mu.Lock()
	ts := s.byVM[t.VM()]
	if ts == nil {
		s.mu.Unlock()
		return
	}
	var stoppers []*Session
	for _, sess := range ts.sessions {
		if sess.state == StateAttached && sess.exceptionBreak {
			stoppers = append(stoppers, sess)
		}
	}
	msg := err.Error()
	if len(stoppers) == 0 {
		for _, sess := range ts.sessions {
			if sess.state == StateAttached {
				sess.events.emit(EventException, t.ID, ReasonException, msg)
			}
		}
		s.mu.Unlock()
		return
	}
	s.beginPauseLocked(ts, t, stoppers, ReasonException)
	s.mu.Unlock()

	for _, sess := range stoppers {
		sess.events.emit(EventException, t.ID, ReasonException, msg)
	}
	<-p_done(ts, t)
	s.finishPause(ts, t)
}

// OnTaskStart notifies sessions of a new execution unit.
func (s *Service) OnTaskStart(t *vm.Task) {
	s.mu.Lock()
	ts := s.byVM[t.VM()]
	var sessions []*Session
	if ts != nil {
		for _, sess := range ts.sessions {
			if sess.state == StateAttached {
				sessions = append(sessions, sess)
			}
		}
	}
	s.mu.Unlock()
	for _, sess := range sessions {
		sess.events.emit(EventTaskStart, t.ID, "", "")
	}
}

// OnTaskEnd notifies sessions that a task finished.
func (s *Service) OnTaskEnd(t *vm.Task) {
	s.mu.Lock()
	ts := s.byVM[t.VM()]
	var sessions []*Session
	if ts != nil {
		for _, sess := range ts.sessions {
			if sess.state == StateAttached {
				sessions = append(sessions, sess)
			}
		}
	}
	s.mu.Unlock()
	for _, sess := range sessions {
		sess.events.emit(EventTaskEnd, t.ID, "", "")
	}
}

// beginPauseLocked registers a pause and snapshots frames per session.
func (s *Service) beginPauseLocked(ts *targetState, t *vm.Task, stoppers []*Session, reason StopReason) {
	p := &pause{task: t, reason: reason, remaining: make(map[int64]bool), done: make(chan struct{})}
	for _, sess := range stoppers {
		p.remaining[sess.id] = true
		sess.pausesHeld[t.ID] = true
		sess.snapshotFramesLocked(t)
	}
	ts.pauses[t.ID] = p
}

// finishPause removes the pause record after the task resumed.
func (s *Service) finishPause(ts *targetState, t *vm.Task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := ts.pauses[t.ID]; ok && len(p.remaining) == 0 {
		delete(ts.pauses, t.ID)
	}
}

// p_done returns the done channel of the active pause for the task.
func p_done(ts *targetState, t *vm.Task) <-chan struct{} {
	return ts.pauses[t.ID].done
}
