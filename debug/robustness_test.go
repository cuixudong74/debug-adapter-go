package debug

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cuixudong74/debug-adapter-go/vm"
)

// TestEvaluateCancellation: a cancelled context aborts evaluation and
// the session stays usable (no intermediate state).
func TestEvaluateCancellation(t *testing.T) {
	svc := NewService()
	v := vm.New(loopProgram(), nil)
	svc.RegisterTarget("app", v)
	sess, _ := svc.Attach(AttachRequest{Target: "app"})
	defer sess.Terminate()
	sess.SetBreakpoints([]Breakpoint{{Function: "main", Line: 3}})

	done := make(chan struct{})
	go func() { v.Run(); close(done) }()
	ev := waitEvent(t, sess, EventStopped, 2*time.Second)
	frames, _ := sess.StackTrace(ev.TaskID)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sess.Evaluate(cancelled, frames[0].ID, "i + 1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled evaluate = %v, want context.Canceled", err)
	}
	// Session is still fully functional afterwards.
	if _, err := sess.Evaluate(testCtx(), frames[0].ID, "i + 1"); err != nil {
		t.Fatalf("evaluate after cancel: %v", err)
	}
	st, _ := sess.TaskState(ev.TaskID)
	if st != "paused" {
		t.Fatalf("task state = %q after cancel, want paused", st)
	}
	sess.Continue(ev.TaskID)
	// Drain.
	for {
		select {
		case <-done:
			return
		case ev := <-sess.Events():
			if ev.Type == EventStopped {
				sess.Continue(ev.TaskID)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("target did not finish")
		}
	}
}

// TestEvaluateErrorsDistinguishable: bad expressions return typed,
// distinguishable errors and never crash the session.
func TestEvaluateErrorsDistinguishable(t *testing.T) {
	svc := NewService()
	v := vm.New(loopProgram(), nil)
	svc.RegisterTarget("app", v)
	sess, _ := svc.Attach(AttachRequest{Target: "app"})
	defer sess.Terminate()
	sess.SetBreakpoints([]Breakpoint{{Function: "main", Line: 3}})

	done := make(chan struct{})
	go func() { v.Run(); close(done) }()
	ev := waitEvent(t, sess, EventStopped, 2*time.Second)
	frames, _ := sess.StackTrace(ev.TaskID)

	if _, err := sess.Evaluate(testCtx(), frames[0].ID, "nosuchvar + 1"); !errors.Is(err, ErrUndefinedSymbol) {
		t.Fatalf("undefined var = %v, want ErrUndefinedSymbol", err)
	}
	if _, err := sess.Evaluate(testCtx(), frames[0].ID, "i +"); err == nil {
		t.Fatal("syntax error expected")
	}
	if _, err := sess.Evaluate(testCtx(), frames[0].ID, "1 / 0"); err == nil ||
		!strings.Contains(err.Error(), "division by zero") {
		t.Fatalf("div by zero = %v", err)
	}
	// Evaluation must not mutate program state: i is unchanged.
	val, err := sess.Evaluate(testCtx(), frames[0].ID, "i")
	if err != nil {
		t.Fatal(err)
	}
	if val != 0 && val != 0.0 {
		t.Fatalf("i = %v after failed evaluations, want 0 (no side effects)", val)
	}
	sess.Continue(ev.TaskID)
	for {
		select {
		case <-done:
			return
		case ev := <-sess.Events():
			if ev.Type == EventStopped {
				sess.Continue(ev.TaskID)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("target did not finish")
		}
	}
}

// TestEventQueueBounded: a slow consumer cannot grow memory without
// bound; drops are reported via an overflow event.
func TestEventQueueBounded(t *testing.T) {
	svc := NewService()
	// No breakpoints; 600 spawned no-op tasks => 1200 events into a
	// 256-slot queue that nobody drains until the target finishes.
	prog2 := &vm.Program{Main: "main", Funcs: map[string]*vm.Function{
		"main": {Name: "main", Code: []vm.Instr{
			{Op: vm.OpConst, Val: 0, Line: 1},
			{Op: vm.OpStore, Name: "n", Line: 1},
			{Op: vm.OpLoad, Name: "n", Line: 2},
			{Op: vm.OpConst, Val: 600, Line: 2},
			{Op: vm.OpLt, Line: 2},
			{Op: vm.OpJumpIfFalse, Target: 12, Line: 2},
			{Op: vm.OpSpawn, Name: "noop", Line: 3},
			{Op: vm.OpLoad, Name: "n", Line: 4},
			{Op: vm.OpConst, Val: 1, Line: 4},
			{Op: vm.OpAdd, Line: 4},
			{Op: vm.OpStore, Name: "n", Line: 4},
			{Op: vm.OpJump, Target: 2, Line: 5},
		}},
		"noop": {Name: "noop", Code: []vm.Instr{
			{Op: vm.OpConst, Val: 1, Line: 10},
			{Op: vm.OpStore, Name: "x", Line: 10},
		}},
	}}
	v2 := vm.New(prog2, nil)
	svc.RegisterTarget("app2", v2)
	sess2, _ := svc.Attach(AttachRequest{Target: "app2"})
	defer sess2.Terminate()

	done := make(chan struct{})
	go func() { v2.Run(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("target must not block on a full event queue")
	}

	// Drain what is buffered: drops are reported via overflow events
	// and the buffered total never exceeds the queue capacity.
	var sawOverflow bool
	var dropped, got int
	for {
		select {
		case ev := <-sess2.Events():
			got++
			if ev.Type == EventOverflow {
				sawOverflow = true
				dropped += ev.Dropped
			}
		default:
			goto drained
		}
	}
drained:
	if !sawOverflow {
		t.Fatal("expected an overflow event after heavy event load")
	}
	if got > eventQueueCap {
		t.Fatalf("received %d buffered events, capacity %d: queue unbounded", got, eventQueueCap)
	}
	if dropped == 0 {
		t.Fatal("overflow event must report dropped count")
	}
	t.Logf("buffered=%d dropped=%d", got, dropped)
}
