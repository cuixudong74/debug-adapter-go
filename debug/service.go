package debug

import (
	"context"
	"sync"

	"github.com/cuixudong74/debug-adapter-go/runtime"
)

// Service manages in-process debug targets and sessions. It is the entry point
// for the editor plugin and developer console.
type Service struct {
	mu       sync.Mutex
	targets  map[string]*Target
	sessions map[string]*Session
	audit    *AuditLog

	yieldMu sync.RWMutex
	yield   func()

	nextTarget  int
	nextSession int
}

// SetYield installs a function applied to every target created after the call.
// It is intended for tests that need to slow execution deterministically.
func (s *Service) SetYield(f func()) {
	s.yieldMu.Lock()
	s.yield = f
	s.yieldMu.Unlock()
}

func (s *Service) getYield() func() {
	s.yieldMu.RLock()
	defer s.yieldMu.RUnlock()
	return s.yield
}

// NewService creates a debug service.
func NewService() *Service {
	return &Service{
		targets:  map[string]*Target{},
		sessions: map[string]*Session{},
		audit:    NewAuditLog(1000),
	}
}

// Audit returns the service audit log.
func (s *Service) Audit() *AuditLog { return s.audit }

// CreateTarget registers a running target without attaching a session. The
// target runs at its own pace until a session attaches.
func (s *Service) CreateTarget(name string, prog *runtime.Program) (*Target, error) {
	s.mu.Lock()
	s.nextTarget++
	id := targetID(s.nextTarget)
	s.mu.Unlock()

	rt := runtime.NewTarget(prog)
	if y := s.getYield(); y != nil {
		rt.SetYield(y)
	}
	if _, err := rt.RunMain("main"); err != nil {
		return nil, err
	}
	t := &Target{
		id:       id,
		name:     name,
		rt:       rt,
		breakpts: newBreakpointRegistry(),
	}
	s.mu.Lock()
	s.targets[id] = t
	s.mu.Unlock()
	return t, nil
}

// createTargetNoRun creates a target without starting its main task. The
// caller must call rt.RunMain after installing the debug hook.
func (s *Service) createTargetNoRun(name string, prog *runtime.Program) (*Target, error) {
	s.mu.Lock()
	s.nextTarget++
	id := targetID(s.nextTarget)
	s.mu.Unlock()

	rt := runtime.NewTarget(prog)
	if y := s.getYield(); y != nil {
		rt.SetYield(y)
	}
	t := &Target{
		id:       id,
		name:     name,
		rt:       rt,
		breakpts: newBreakpointRegistry(),
	}
	s.mu.Lock()
	s.targets[id] = t
	s.mu.Unlock()
	return t, nil
}

// Launch creates a target, attaches a session (installing the debug hook), and
// then starts the main task so the hook is in place before the first line.
func (s *Service) Launch(ctx context.Context, name string, prog *runtime.Program, user User) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, newError(KindCanceled, "launch canceled: "+err.Error())
	}
	t, err := s.createTargetNoRun(name, prog)
	if err != nil {
		return nil, err
	}
	sess, err := s.Attach(ctx, t.id, user)
	if err != nil {
		return nil, err
	}
	if _, err := t.rt.RunMain("main"); err != nil {
		return nil, err
	}
	return sess, nil
}

// Attach attaches a new session to a running target. Attach is exclusive: a
// target has at most one attached session. Breakpoints persist across detach
// and are restored automatically.
func (s *Service) Attach(ctx context.Context, targetID string, user User) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, newError(KindCanceled, "attach canceled: "+err.Error())
	}
	s.mu.Lock()
	t, ok := s.targets[targetID]
	if !ok {
		s.mu.Unlock()
		return nil, errorf(KindNotFound, "target %s not found", targetID)
	}
	s.nextSession++
	sid := sessionID(s.nextSession)
	s.mu.Unlock()

	t.mu.Lock()
	if t.session != nil {
		t.mu.Unlock()
		return nil, errorf(KindAlreadyAttached, "target %s already has an attached session", targetID)
	}
	sess := &Session{
		id:        sid,
		targetID:  t.id,
		target:    t,
		user:      user,
		svc:       s,
		state:     stateRunning,
		steps:     map[int]*stepState{},
		refs:      newRefTable(),
		frames:    map[int]*runtime.Frame{},
		frameRefs: map[int]int{},
		exFilter:  ExNone,
		events:    NewEventQueue(256),
	}
	t.session = sess
	t.mu.Unlock()

	s.mu.Lock()
	s.sessions[sid] = sess
	s.mu.Unlock()

	t.rt.SetHook(sess)
	s.audit.Record(AuditEntry{
		User: user.ID, Action: "session:attach", SessionID: sid,
		TargetID: t.id, Granted: true,
	})
	return sess, nil
}

// Target returns the target with the given id.
func (s *Service) Target(id string) (*Target, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.targets[id]
	return t, ok
}

// SessionByID returns the session with the given id.
func (s *Service) SessionByID(id string) (*Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	return sess, ok
}

// Targets returns all known targets.
func (s *Service) Targets() []*Target {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Target, 0, len(s.targets))
	for _, t := range s.targets {
		out = append(out, t)
	}
	return out
}

// Sessions returns all known sessions.
func (s *Service) Sessions() []*Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, sess)
	}
	return out
}

func (s *Service) detachSession(sess *Session) {
	s.mu.Lock()
	delete(s.sessions, sess.id)
	s.mu.Unlock()
}

func (s *Service) finishSession(sess *Session) {
	s.mu.Lock()
	delete(s.sessions, sess.id)
	if t, ok := s.targets[sess.targetID]; ok {
		t.mu.Lock()
		t.session = nil
		t.mu.Unlock()
	}
	s.mu.Unlock()
}

func targetID(n int) string  { return fmtID("target", n) }
func sessionID(n int) string { return fmtID("session", n) }

func fmtID(prefix string, n int) string {
	return prefix + "-" + itoa(n)
}
