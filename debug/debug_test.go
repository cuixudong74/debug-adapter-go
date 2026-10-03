package debug

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cuixudong74/debug-adapter-go/runtime"
)

func testUser(scopes ...Scope) User {
	roles := make([]string, len(scopes))
	for i, s := range scopes {
		roles[i] = string(s)
	}
	return User{ID: "u", Name: "tester", Roles: roles}
}

func fullUser() User {
	return testUser(ScopeControl, ScopeReadVariables, ScopeEvaluate, ScopeReadSensitive)
}

func launch(t *testing.T, svc *Service, name string, prog *runtime.Program, user User) *Session {
	t.Helper()
	// slow every target so breakpoints can be set before the task runs away
	svc.SetYield(func() { time.Sleep(time.Millisecond) })
	sess, err := svc.Launch(context.Background(), name, prog, user)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	return sess
}

func waitEvent(t *testing.T, sess *Session, timeout time.Duration) *Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ev, err := sess.Events().Next(ctx)
	if err != nil {
		t.Fatalf("wait event: %v", err)
	}
	return ev
}

func waitStop(t *testing.T, sess *Session) *Event {
	t.Helper()
	for {
		ev := waitEvent(t, sess, 3*time.Second)
		if ev.Kind == EvStopped {
			return ev
		}
	}
}

func waitTerminated(t *testing.T, sess *Session) *Event {
	t.Helper()
	for {
		ev := waitEvent(t, sess, 3*time.Second)
		if ev.Kind == EvTerminated {
			return ev
		}
	}
}

// drainEvents removes all currently queued events.
func drainEvents(sess *Session) {
	sess.Events().Drain()
}

// ---------- 1. concurrent sessions isolation ----------

func TestConcurrentSessionsIsolated(t *testing.T) {
	svc := NewService()
	prog1 := runtime.NewProgram("p1")
	prog1.Func("main", "m.go").Nop(1).Nop(2).Nop(3).Return(4)
	prog2 := runtime.NewProgram("p2")
	prog2.Func("main", "m.go").Nop(1).Nop(2).Return(3)

	s1 := launch(t, svc, "t1", prog1, fullUser())
	s2 := launch(t, svc, "t2", prog2, fullUser())

	if _, err := s1.SetBreakpoints(context.Background(), "m.go", []BreakpointSpec{{Line: 2}}); err != nil {
		t.Fatal(err)
	}
	// s2 has no breakpoints and runs to termination
	waitTerminated(t, s2)
	if s2.State() != stateTerminated {
		t.Fatalf("s2 should be terminated, got %s", s2.State())
	}
	// s1 is paused at its breakpoint, unaffected by s2
	ev := waitStop(t, s1)
	if ev.Reason != ReasonBreakpoint {
		t.Fatalf("s1 should be stopped by breakpoint, got %s", ev.Reason)
	}
	if s1.State() != statePaused {
		t.Fatalf("s1 should be paused, got %s", s1.State())
	}
	// s1's breakpoint set is independent
	bps, err := s1.Breakpoints(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(bps) != 1 {
		t.Fatalf("s1 should have 1 breakpoint, got %d", len(bps))
	}
	bps2, err := s2.Breakpoints(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(bps2) != 0 {
		t.Fatalf("s2 should have 0 breakpoints, got %d", len(bps2))
	}
	// s2's queue never saw a stopped event
	for _, e := range s2.Events().Drain() {
		if e.Kind == EvStopped {
			t.Fatalf("s2 must not receive stopped events, got %+v", e)
		}
	}
}

// ---------- 2. breakpoint idempotency + recovery ----------

func TestBreakpointIdempotent(t *testing.T) {
	svc := NewService()
	prog := runtime.NewProgram("p")
	prog.Func("main", "m.go").Nop(1).Return(2)
	sess := launch(t, svc, "t", prog, fullUser())

	b1, err := sess.SetBreakpoints(context.Background(), "m.go", []BreakpointSpec{{Line: 1}})
	if err != nil {
		t.Fatal(err)
	}
	b2, err := sess.SetBreakpoints(context.Background(), "m.go", []BreakpointSpec{{Line: 1, Condition: "x > 0"}})
	if err != nil {
		t.Fatal(err)
	}
	if b1[0].ID != b2[0].ID {
		t.Fatalf("re-setting a breakpoint at the same location must be idempotent: %d vs %d", b1[0].ID, b2[0].ID)
	}
	// delete twice is a no-op
	if err := sess.DeleteBreakpoints(context.Background(), []int{b1[0].ID}); err != nil {
		t.Fatal(err)
	}
	if err := sess.DeleteBreakpoints(context.Background(), []int{b1[0].ID}); err != nil {
		t.Fatal(err)
	}
	bps, _ := sess.Breakpoints(context.Background())
	if len(bps) != 0 {
		t.Fatalf("breakpoint should be removed, got %d", len(bps))
	}
}

func TestBreakpointRecoveryOnReattach(t *testing.T) {
	svc := NewService()
	prog := runtime.NewProgram("p")
	// bounded loop: line 2 executes 3 times
	prog.Func("main", "m.go").Nop(1).Nop(2).Loop(3, 2, 3).Nop(4).Return(5)
	sess := launch(t, svc, "t", prog, fullUser())
	var yields int64
	sess.target.rt.SetYield(func() {
		atomic.AddInt64(&yields, 1)
		time.Sleep(2 * time.Millisecond)
	})

	if _, err := sess.SetBreakpoints(context.Background(), "m.go", []BreakpointSpec{{Line: 2}}); err != nil {
		t.Fatal(err)
	}
	ev := waitStop(t, sess)
	if ev.Reason != ReasonBreakpoint {
		t.Fatalf("expected breakpoint stop, got %s", ev.Reason)
	}
	// detach: target keeps running at its own pace
	if err := sess.Detach(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sess.State() != stateDetached {
		t.Fatalf("expected detached, got %s", sess.State())
	}
	main := sess.target.rt.Main()
	if main.IsDone() {
		t.Fatal("target must not terminate on detach")
	}
	// re-attach: breakpoint set is restored
	sess2, err := svc.Attach(context.Background(), sess.targetID, fullUser())
	if err != nil {
		t.Fatal(err)
	}
	bps, err := sess2.Breakpoints(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(bps) != 1 || bps[0].Line != 2 {
		t.Fatalf("breakpoint set not restored on re-attach: %+v", bps)
	}
	// the target is still running and will hit the restored breakpoint exactly once more
	ev2 := waitStop(t, sess2)
	if ev2.Reason != ReasonBreakpoint {
		t.Fatalf("expected breakpoint stop after re-attach, got %s", ev2.Reason)
	}
	// the restored breakpoint hits exactly once after re-attach (no duplicate
	// pause caused by the re-attach itself)
	if bps[0].HitCount != 2 {
		t.Fatalf("expected 2 total hits (one per line entry, no duplicates), got %d", bps[0].HitCount)
	}
	// remove the breakpoint; the loop finishes without further stops
	if err := sess2.DeleteBreakpoints(context.Background(), []int{bps[0].ID}); err != nil {
		t.Fatal(err)
	}
	if err := sess2.Continue(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitTerminated(t, sess2)
	if sess2.State() != stateTerminated {
		t.Fatalf("expected terminated, got %s", sess2.State())
	}
}

func TestDetachedSessionReceivesNoEvents(t *testing.T) {
	svc := NewService()
	prog := runtime.NewProgram("p")
	prog.Func("main", "m.go").Nop(1).Loop(2, 1, 100).Return(3)
	sess := launch(t, svc, "t", prog, fullUser())
	sess.target.rt.SetYield(func() { time.Sleep(time.Millisecond) })
	if err := sess.Detach(context.Background()); err != nil {
		t.Fatal(err)
	}
	// queue is closed; Next reports termination
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := sess.Events().Next(ctx); err == nil {
		t.Fatal("detached session must not receive events")
	}
}

// ---------- 3. stepping boundaries ----------

func steppingProgram() *runtime.Program {
	p := runtime.NewProgram("step")
	p.Func("main", "m.go").
		Nop(1).
		Call(2, "helper").
		Nop(3).
		Return(4)
	p.Func("helper", "h.go").
		Nop(1).
		Nop(2).
		Return(3)
	p.Func("worker", "w.go").
		Loop(1, 1, 200).
		Return(2)
	return p
}

func TestStepOverDoesNotEnterCallee(t *testing.T) {
	svc := NewService()
	sess := launch(t, svc, "t", steppingProgram(), fullUser())
	sess.SetBreakpoints(context.Background(), "m.go", []BreakpointSpec{{Line: 1}})
	ev := waitStop(t, sess)
	taskID := ev.TaskID

	// step over line 1 -> stops at line 2 (call), still in main
	if err := sess.StepOver(context.Background(), taskID); err != nil {
		t.Fatal(err)
	}
	ev = waitStop(t, sess)
	frames, err := sess.Stack(context.Background(), ev.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if frames[0].Name != "main" || frames[0].Line != 2 {
		t.Fatalf("step over should stop at main:2, got %s:%d", frames[0].Name, frames[0].Line)
	}

	// step over the call -> stops at main:3, never inside helper
	if err := sess.StepOver(context.Background(), taskID); err != nil {
		t.Fatal(err)
	}
	ev = waitStop(t, sess)
	frames, err = sess.Stack(context.Background(), ev.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if frames[0].Name != "main" || frames[0].Line != 3 {
		t.Fatalf("step over call should stop at main:3, got %s:%d", frames[0].Name, frames[0].Line)
	}
	if len(frames) != 1 {
		t.Fatalf("step over must not enter helper, got %d frames", len(frames))
	}
}

func TestStepInAndOut(t *testing.T) {
	svc := NewService()
	sess := launch(t, svc, "t", steppingProgram(), fullUser())
	sess.SetBreakpoints(context.Background(), "m.go", []BreakpointSpec{{Line: 2}})
	ev := waitStop(t, sess)
	taskID := ev.TaskID

	// step in -> enters helper
	if err := sess.StepIn(context.Background(), taskID); err != nil {
		t.Fatal(err)
	}
	ev = waitStop(t, sess)
	frames, err := sess.Stack(context.Background(), ev.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if frames[0].Name != "helper" || frames[0].Line != 1 {
		t.Fatalf("step in should stop at helper:1, got %s:%d", frames[0].Name, frames[0].Line)
	}
	if len(frames) != 2 {
		t.Fatalf("step in should have 2 frames, got %d", len(frames))
	}

	// step out -> back to main, at the line after the call
	if err := sess.StepOut(context.Background(), taskID); err != nil {
		t.Fatal(err)
	}
	ev = waitStop(t, sess)
	frames, err = sess.Stack(context.Background(), ev.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if frames[0].Name != "main" || frames[0].Line != 3 {
		t.Fatalf("step out should stop at main:3, got %s:%d", frames[0].Name, frames[0].Line)
	}
	if len(frames) != 1 {
		t.Fatalf("step out should return to 1 frame, got %d", len(frames))
	}
}

func TestPausedTaskDoesNotAdvance(t *testing.T) {
	svc := NewService()
	sess := launch(t, svc, "t", steppingProgram(), fullUser())
	sess.SetBreakpoints(context.Background(), "m.go", []BreakpointSpec{{Line: 1}})
	ev := waitStop(t, sess)
	line := sess.target.rt.Main().Top().Line
	time.Sleep(50 * time.Millisecond)
	if sess.target.rt.Main().Top().Line != line {
		t.Fatalf("paused task advanced: line %d -> %d", line, sess.target.rt.Main().Top().Line)
	}
	if !sess.target.rt.Main().IsPaused() {
		t.Fatal("paused task should stay paused")
	}
	_ = ev
}

func TestSteppingDoesNotStopOtherTasks(t *testing.T) {
	svc := NewService()
	sess := launch(t, svc, "t", steppingProgram(), fullUser())
	var yields int64
	sess.target.rt.SetYield(func() {
		atomic.AddInt64(&yields, 1)
		time.Sleep(time.Millisecond)
	})
	sess.SetBreakpoints(context.Background(), "m.go", []BreakpointSpec{{Line: 1}})
	ev := waitStop(t, sess)
	taskID := ev.TaskID

	// spawn a concurrent worker
	worker, err := sess.target.rt.Spawn("worker", "worker")
	if err != nil {
		t.Fatal(err)
	}
	// step main while worker runs
	for i := 0; i < 3; i++ {
		if err := sess.StepOver(context.Background(), taskID); err != nil {
			t.Fatal(err)
		}
		waitStop(t, sess)
	}
	// worker must never have been paused by main's stepping
	if worker.IsPaused() {
		t.Fatal("worker must not be paused while stepping main")
	}
	if atomic.LoadInt64(&yields) == 0 {
		t.Fatal("worker should have advanced while main was stepped")
	}
	if sess.State() != statePaused {
		t.Fatalf("session should be paused after stepping, got %s", sess.State())
	}
}

// ---------- 4. exceptions ----------

func exceptionProgram() *runtime.Program {
	p := runtime.NewProgram("exc")
	p.Func("main", "m.go").
		Nop(1).
		Throw(2, runtime.ObjectValue(runtime.Field{Name: "msg", Value: runtime.StringValue("boom")})).
		Return(3)
	return p
}

func TestUncaughtExceptionTerminates(t *testing.T) {
	svc := NewService()
	sess := launch(t, svc, "t", exceptionProgram(), fullUser())
	ev := waitTerminated(t, sess)
	if ev.Exception == "" {
		t.Fatal("terminated event should carry exception info")
	}
	if sess.State() != stateTerminated {
		t.Fatalf("expected terminated, got %s", sess.State())
	}
	ex, err := sess.LastException(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ex.Type != "object" {
		t.Fatalf("exception should be an object, got %s", ex.Type)
	}
}

func TestExceptionBreakpointPauses(t *testing.T) {
	svc := NewService()
	sess := launch(t, svc, "t", exceptionProgram(), fullUser())
	sess.target.rt.SetYield(func() { time.Sleep(2 * time.Millisecond) })
	if err := sess.SetExceptionFilter(context.Background(), ExUncaught); err != nil {
		t.Fatal(err)
	}
	ev := waitStop(t, sess)
	if ev.Reason != ReasonException {
		t.Fatalf("expected exception stop, got %s", ev.Reason)
	}
	if sess.State() != statePaused {
		t.Fatalf("expected paused, got %s", sess.State())
	}
	frames, err := sess.Stack(context.Background(), ev.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if frames[0].Line != 2 {
		t.Fatalf("should pause at throw line 2, got %d", frames[0].Line)
	}
	ex, err := sess.LastException(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ex.Type != "object" {
		t.Fatalf("exception should be object, got %s", ex.Type)
	}
	// after continue, the task dies and the target terminates
	if err := sess.Continue(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitTerminated(t, sess)
	if sess.State() != stateTerminated {
		t.Fatalf("expected terminated, got %s", sess.State())
	}
}

func TestFatalCrashTerminates(t *testing.T) {
	svc := NewService()
	p := runtime.NewProgram("crash")
	p.Func("main", "m.go").Nop(1).Fatal(2, "segfault").Return(3)
	sess := launch(t, svc, "t", p, fullUser())
	ev := waitTerminated(t, sess)
	if !ev.Fatal {
		t.Fatal("terminated event should mark fatal crash")
	}
	if ev.Exception == "" {
		t.Fatal("fatal crash should carry error info")
	}
	if sess.State() != stateTerminated {
		t.Fatalf("expected terminated, got %s", sess.State())
	}
}

// ---------- 5. timeout / cancel / queue ----------

func TestCanceledEvaluateLeavesNoIntermediateState(t *testing.T) {
	svc := NewService()
	sess := launch(t, svc, "t", steppingProgram(), fullUser())
	sess.SetBreakpoints(context.Background(), "m.go", []BreakpointSpec{{Line: 1}})
	waitStop(t, sess)
	frames, err := sess.Stack(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = sess.Evaluate(ctx, "1+1", frames[0].ID)
	if !IsKind(err, KindCanceled) {
		t.Fatalf("expected canceled, got %v", err)
	}
	// session is still paused and usable
	if sess.State() != statePaused {
		t.Fatalf("session should still be paused, got %s", sess.State())
	}
	v, err := sess.Evaluate(context.Background(), "1+1", frames[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Value != "2" {
		t.Fatalf("expected 2, got %s", v.Value)
	}
}

func TestEventQueueBoundedAndCriticalNeverDropped(t *testing.T) {
	q := NewEventQueue(4)
	for i := 0; i < 10; i++ {
		q.Emit(&Event{Kind: EvStopped, TaskID: i})
	}
	if q.Len() > 4 {
		t.Fatalf("queue exceeded capacity: %d", q.Len())
	}
	if q.Dropped() == 0 {
		t.Fatal("expected drops under overflow")
	}
	// terminated events are never dropped
	q2 := NewEventQueue(2)
	q2.Emit(&Event{Kind: EvStopped, TaskID: 1})
	q2.Emit(&Event{Kind: EvStopped, TaskID: 2})
	q2.Emit(&Event{Kind: EvTerminated})
	found := false
	for _, e := range q2.Drain() {
		if e.Kind == EvTerminated {
			found = true
		}
	}
	if !found {
		t.Fatal("terminated event must not be dropped")
	}
}

func TestEventQueueOutOfOrderAndDuplicates(t *testing.T) {
	q := NewEventQueue(10)
	// emit out of order with explicit seqs
	q.emitAt(&Event{Kind: EvStopped, TaskID: 3})
	q.emitAt(&Event{Kind: EvStopped, TaskID: 1})
	q.emitAt(&Event{Kind: EvStopped, TaskID: 2})
	ctx := context.Background()
	first, _ := q.Next(ctx)
	if first.TaskID != 3 {
		t.Fatalf("expected FIFO arrival order (3), got %d", first.TaskID)
	}
	// drain the rest
	for q.Len() > 0 {
		q.Next(ctx)
	}
	// duplicate seq does not corrupt the queue
	q.emitAt(&Event{Kind: EvStopped, TaskID: 99})
	q.emitAt(&Event{Kind: EvStopped, TaskID: 99})
	a, _ := q.Next(ctx)
	b, _ := q.Next(ctx)
	if a.TaskID != 99 || b.TaskID != 99 {
		t.Fatalf("duplicate events should both be delivered, got %d/%d", a.TaskID, b.TaskID)
	}
	if q.Len() != 0 {
		t.Fatalf("queue should be empty, got %d", q.Len())
	}
}

func TestStaleEventsDoNotCorruptSession(t *testing.T) {
	svc := NewService()
	sess := launch(t, svc, "t", steppingProgram(), fullUser())
	sess.SetBreakpoints(context.Background(), "m.go", []BreakpointSpec{{Line: 1}})
	waitStop(t, sess)

	// inject out-of-order / duplicate events; session state must not change
	sess.events.emitAt(&Event{Kind: EvContinued, TaskID: 1})
	sess.events.emitAt(&Event{Kind: EvStopped, TaskID: 1})
	sess.events.emitAt(&Event{Kind: EvTerminated})
	if sess.State() != statePaused {
		t.Fatalf("session state corrupted by injected events: %s", sess.State())
	}
	// session is still operable
	if err := sess.Continue(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// ---------- 6. permissions ----------

func permissionProgram() *runtime.Program {
	p := runtime.NewProgram("sec")
	obj := runtime.ObjectValue(
		runtime.SensitiveField("password", runtime.StringValue("hunter2")),
		runtime.Field{Name: "user", Value: runtime.StringValue("bob")},
		runtime.Field{Name: "nested", Value: runtime.ObjectValue(runtime.Field{Name: "x", Value: runtime.IntValue(1)})},
	)
	p.Func("main", "m.go").Nop(1).Return(2)
	p.Global("cfg", obj)
	return p
}

func TestSensitiveVariableReadDeniedAndAudited(t *testing.T) {
	svc := NewService()
	// user has read/evaluate but NOT read_sensitive
	user := testUser(ScopeControl, ScopeReadVariables, ScopeEvaluate)
	sess := launch(t, svc, "t", permissionProgram(), user)
	sess.SetBreakpoints(context.Background(), "m.go", []BreakpointSpec{{Line: 1}})
	ev := waitStop(t, sess)
	frames, err := sess.Stack(context.Background(), ev.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	// evaluate cfg -> object variable
	v, err := sess.Evaluate(context.Background(), "cfg", frames[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.VariablesReference == 0 {
		t.Fatal("cfg should be an object with a variablesReference")
	}
	// reading children: non-sensitive fields ok, sensitive denied
	_, err = sess.Variables(context.Background(), v.VariablesReference)
	if !IsKind(err, KindPermission) {
		t.Fatalf("expected permission denied, got %v", err)
	}
	// evaluate referencing the sensitive name is also denied
	_, err = sess.Evaluate(context.Background(), "cfg.password", frames[0].ID)
	if !IsKind(err, KindPermission) {
		t.Fatalf("expected permission denied for sensitive eval, got %v", err)
	}
	// audit recorded the denials
	entries := svc.Audit().Entries()
	denied := 0
	for _, e := range entries {
		if !e.Granted {
			denied++
		}
	}
	if denied < 2 {
		t.Fatalf("expected at least 2 denied audit entries, got %d", denied)
	}
}

func TestSensitiveVariableReadAllowedWithScope(t *testing.T) {
	svc := NewService()
	sess := launch(t, svc, "t", permissionProgram(), fullUser())
	sess.SetBreakpoints(context.Background(), "m.go", []BreakpointSpec{{Line: 1}})
	ev := waitStop(t, sess)
	frames, err := sess.Stack(context.Background(), ev.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	v, err := sess.Evaluate(context.Background(), "cfg", frames[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	children, err := sess.Variables(context.Background(), v.VariablesReference)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 3 {
		t.Fatalf("expected 2 children, got %d", len(children))
	}
	found := false
	for _, c := range children {
		if c.Name == "password" && c.Value != "\"hunter2\"" {
			t.Fatalf("password value mismatch: %s", c.Value)
		}
		if c.Name == "password" {
			found = true
		}
	}
	if !found {
		t.Fatal("password field not returned")
	}
}

func TestNestedVariablesLazyAndPure(t *testing.T) {
	svc := NewService()
	prog := runtime.NewProgram("nested")
	nested := runtime.ObjectValue(runtime.Field{Name: "x", Value: runtime.IntValue(42)})
	obj := runtime.ObjectValue(
		runtime.Field{Name: "nested", Value: nested},
		runtime.Field{Name: "arr", Value: runtime.ArrayValue(runtime.IntValue(1), runtime.IntValue(2))},
	)
	prog.Func("main", "m.go").Nop(1).Return(2)
	prog.Global("cfg", obj)
	sess := launch(t, svc, "t", prog, fullUser())
	sess.SetBreakpoints(context.Background(), "m.go", []BreakpointSpec{{Line: 1}})
	ev := waitStop(t, sess)
	frames, _ := sess.Stack(context.Background(), ev.TaskID)
	v, err := sess.Evaluate(context.Background(), "cfg", frames[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	children, err := sess.Variables(context.Background(), v.VariablesReference)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 2 {
		t.Fatalf("expected 2 children, got %d", len(children))
	}
	// nested object has its own reference; lazy resolution
	for _, c := range children {
		if c.Name == "nested" {
			inner, err := sess.Variables(context.Background(), c.VariablesReference)
			if err != nil {
				t.Fatal(err)
			}
			if len(inner) != 1 || inner[0].Value != "42" {
				t.Fatalf("nested var mismatch: %+v", inner)
			}
		}
	}
	// evaluation does not change state: re-evaluate
	v2, err := sess.Evaluate(context.Background(), "cfg.nested.x", frames[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Value != "42" {
		t.Fatalf("expected 42, got %s", v2.Value)
	}
}

// ---------- 7. conditional breakpoints ----------

func TestConditionalBreakpointDeterministic(t *testing.T) {
	svc := NewService()
	prog := runtime.NewProgram("cond")
	// loop line 2 three times; counter is not available, so use a global
	prog.Func("main", "m.go").Nop(1).Nop(2).Loop(3, 2, 3).Nop(4).Return(5)
	prog.Global("threshold", runtime.IntValue(2))
	sess := launch(t, svc, "t", prog, fullUser())
	sess.target.rt.SetYield(func() { time.Sleep(2 * time.Millisecond) })

	// condition "threshold > 1" is always true -> pauses every iteration
	if _, err := sess.SetBreakpoints(context.Background(), "m.go", []BreakpointSpec{{Line: 2, Condition: "threshold > 1"}}); err != nil {
		t.Fatal(err)
	}
	// first hit
	ev := waitStop(t, sess)
	if ev.Reason != ReasonBreakpoint {
		t.Fatalf("expected breakpoint, got %s", ev.Reason)
	}
	// continue -> second hit (condition still true)
	sess.Continue(context.Background())
	ev = waitStop(t, sess)
	if ev.Reason != ReasonBreakpoint {
		t.Fatalf("expected second breakpoint hit, got %s", ev.Reason)
	}
	// continue -> third hit
	sess.Continue(context.Background())
	ev = waitStop(t, sess)
	if ev.Reason != ReasonBreakpoint {
		t.Fatalf("expected third breakpoint hit, got %s", ev.Reason)
	}
	// remove breakpoint, finish
	sess.DeleteBreakpoints(context.Background(), []int{ev.BreakpointID})
	sess.Continue(context.Background())
	waitTerminated(t, sess)
}

func TestConditionalBreakpointFalseSkips(t *testing.T) {
	svc := NewService()
	prog := runtime.NewProgram("cond")
	prog.Func("main", "m.go").Nop(1).Nop(2).Loop(3, 2, 3).Nop(4).Return(5)
	prog.Global("threshold", runtime.IntValue(0))
	sess := launch(t, svc, "t", prog, fullUser())
	sess.target.rt.SetYield(func() { time.Sleep(2 * time.Millisecond) })

	// condition "threshold > 1" is false -> never pauses, runs to termination
	if _, err := sess.SetBreakpoints(context.Background(), "m.go", []BreakpointSpec{{Line: 2, Condition: "threshold > 1"}}); err != nil {
		t.Fatal(err)
	}
	waitTerminated(t, sess)
	if sess.State() != stateTerminated {
		t.Fatalf("expected terminated, got %s", sess.State())
	}
}

func TestConditionalBreakpointEvalErrorDoesNotCrash(t *testing.T) {
	svc := NewService()
	prog := runtime.NewProgram("cond")
	prog.Func("main", "m.go").Nop(1).Nop(2).Return(3)
	sess := launch(t, svc, "t", prog, fullUser())
	// condition references an undefined name -> eval error, must not crash
	if _, err := sess.SetBreakpoints(context.Background(), "m.go", []BreakpointSpec{{Line: 2, Condition: "no_such_var > 0"}}); err != nil {
		t.Fatal(err)
	}
	// target runs to termination without pausing; collect events along the way
	found := false
	for {
		ev, err := sess.Events().Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == EvDiagnostic {
			found = true
		}
		if ev.Kind == EvTerminated {
			break
		}
	}
	if !found {
		t.Fatal("expected a diagnostic event for condition eval failure")
	}
	if sess.State() != stateTerminated {
		t.Fatalf("expected terminated, got %s", sess.State())
	}
}
