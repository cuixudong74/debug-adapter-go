package debug

import (
	"testing"
	"time"

	"github.com/cuixudong74/debug-adapter-go/vm"
)

// TestConcurrentSessionsIsolation: two sessions on one target have
// independent breakpoints; each only receives its own stops, and both
// must resume before the task continues.
func TestConcurrentSessionsIsolation(t *testing.T) {
	svc := NewService()
	v := vm.New(loopProgram(), nil)
	svc.RegisterTarget("app", v)

	a, _ := svc.Attach(AttachRequest{Target: "app"})
	b, _ := svc.Attach(AttachRequest{Target: "app"})

	// A breaks on line 3 (loop body), B on line 2 (loop head).
	a.SetBreakpoints([]Breakpoint{{Function: "main", Line: 3}})
	b.SetBreakpoints([]Breakpoint{{Function: "main", Line: 2}})

	done := make(chan struct{})
	go func() { v.Run(); close(done) }()

	// B stops at line 2 first (first instruction of the program run
	// hits line 1... line 2 is the loop head, hit 4 times).
	evB := waitEvent(t, b, EventStopped, 2*time.Second)
	if evB.Reason != ReasonBreakpoint {
		t.Fatalf("B reason = %s", evB.Reason)
	}
	// A must not have received B's stop. The task only advances once
	// B resumes it; then A's own breakpoint at line 3 fires.
	b.Continue(evB.TaskID)
	evA := waitEvent(t, a, EventStopped, 2*time.Second)
	framesA, err := a.StackTrace(evA.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if framesA[0].Line != 3 {
		t.Fatalf("A stopped at line %d, want 3 (breakpoints must be isolated)", framesA[0].Line)
	}
	a.Continue(evA.TaskID)

	// Drain: keep resuming both until the program finishes.
	stops := map[int64]int{a.ID(): 0, b.ID(): 0}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-done:
			if stops[a.ID()] == 0 || stops[b.ID()] == 0 {
				t.Fatalf("each session should have stopped: %v", stops)
			}
			a.Terminate()
			b.Terminate()
			return
		case ev := <-a.Events():
			if ev.Type == EventStopped {
				stops[a.ID()]++
				a.Continue(ev.TaskID)
			}
		case ev := <-b.Events():
			if ev.Type == EventStopped {
				stops[b.ID()]++
				b.Continue(ev.TaskID)
			}
		case <-deadline:
			t.Fatal("program did not finish; sessions likely interfered")
		}
	}
}

// TestDetachKeepsTargetRunning: detaching a session that holds a pause
// releases the pause; the target finishes and its state is unaffected.
func TestDetachKeepsTargetRunning(t *testing.T) {
	svc := NewService()
	v := vm.New(loopProgram(), nil)
	svc.RegisterTarget("app", v)
	sess, _ := svc.Attach(AttachRequest{Target: "app"})
	sess.SetBreakpoints([]Breakpoint{{Function: "main", Line: 3}})

	done := make(chan struct{})
	go func() { v.Run(); close(done) }()

	ev := waitEvent(t, sess, EventStopped, 2*time.Second)
	st, _ := sess.TaskState(ev.TaskID)
	if st != "paused" {
		t.Fatalf("state = %s", st)
	}
	sess.Detach() // must release the pause

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("target did not finish after detach")
	}
	// Detached session receives no further events; channel is closed.
	if _, ok := <-sess.Events(); ok {
		t.Fatal("detached session must not receive events")
	}
	if err := sess.Continue(1); err != ErrSessionClosed {
		t.Fatalf("Continue after detach = %v", err)
	}
	// Semantic state unaffected: task ran to completion without error.
	for _, task := range v.Tasks() {
		if task.Err() != nil {
			t.Fatalf("task %d err after detach: %v", task.ID, task.Err())
		}
	}
}

// TestBreakpointRestoreOnReattach: breakpoints persist in the target
// profile and are restored when a new session attaches.
func TestBreakpointRestoreOnReattach(t *testing.T) {
	svc := NewService()
	v := vm.New(loopProgram(), nil)
	svc.RegisterTarget("app", v)

	s1, _ := svc.Attach(AttachRequest{Target: "app"})
	bps, _ := s1.SetBreakpoints([]Breakpoint{{Function: "main", Line: 3, Condition: "i > 0"}})
	s1.Detach()

	s2, err := svc.Attach(AttachRequest{Target: "app"})
	if err != nil {
		t.Fatal(err)
	}
	restored := s2.Breakpoints()
	if len(restored) != 1 || restored[0].Line != 3 || restored[0].Condition != "i > 0" {
		t.Fatalf("restored = %+v", restored)
	}
	if restored[0].ID != bps[0].ID {
		t.Fatalf("restored ID = %d, want stable ID %d", restored[0].ID, bps[0].ID)
	}

	// The restored conditional breakpoint must actually fire (i>0).
	done := make(chan struct{})
	go func() { v.Run(); close(done) }()
	ev := waitEvent(t, s2, EventStopped, 2*time.Second)
	frames, _ := s2.StackTrace(ev.TaskID)
	val, err := s2.Evaluate(testCtx(), frames[0].ID, "i")
	if err != nil {
		t.Fatal(err)
	}
	if val != 1.0 { // condition i>0 skips the i==0 iteration
		t.Fatalf("first hit at i=%v, want 1 (condition must apply)", val)
	}
	s2.Continue(ev.TaskID)
	// Drain remaining stops.
	for {
		select {
		case <-done:
			s2.Terminate()
			return
		case ev := <-s2.Events():
			if ev.Type == EventStopped {
				s2.Continue(ev.TaskID)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("target did not finish")
		}
	}
}

// TestBreakpointIdempotency: repeated set/remove are no-ops.
func TestBreakpointIdempotency(t *testing.T) {
	svc := NewService()
	v := vm.New(loopProgram(), nil)
	svc.RegisterTarget("app", v)
	sess, _ := svc.Attach(AttachRequest{Target: "app"})
	defer sess.Terminate()

	first, _ := sess.SetBreakpoints([]Breakpoint{{Function: "main", Line: 2}})
	second, _ := sess.SetBreakpoints([]Breakpoint{{Function: "main", Line: 2}})
	if first[0].ID != second[0].ID {
		t.Fatalf("IDs differ across identical sets: %d vs %d", first[0].ID, second[0].ID)
	}
	if got := len(sess.Breakpoints()); got != 1 {
		t.Fatalf("breakpoints = %d, want 1 (no duplicates)", got)
	}
	sess.RemoveBreakpoint(first[0].ID)
	sess.RemoveBreakpoint(first[0].ID) // missing ID: no-op
	sess.RemoveBreakpoint(9999)        // unknown ID: no-op
	if got := len(sess.Breakpoints()); got != 0 {
		t.Fatalf("breakpoints = %d, want 0", got)
	}
}
