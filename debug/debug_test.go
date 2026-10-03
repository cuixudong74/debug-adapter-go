package debug

import (
	"context"
	"testing"
	"time"

	"github.com/cuixudong74/debug-adapter-go/vm"
)

// loopProgram builds: main: i=0; loop: i=i+1 (lines 1..3), spawn worker.
func loopProgram() *vm.Program {
	return &vm.Program{Main: "main", Funcs: map[string]*vm.Function{
		"main": {Name: "main", Code: []vm.Instr{
			{Op: vm.OpConst, Val: 0, Line: 1},
			{Op: vm.OpStore, Name: "i", Line: 1},
			// line 2: loop head: i < 3 ?
			{Op: vm.OpLoad, Name: "i", Line: 2},
			{Op: vm.OpConst, Val: 3, Line: 2},
			{Op: vm.OpLt, Line: 2},
			{Op: vm.OpJumpIfFalse, Target: 11, Line: 2},
			// line 3: i = i + 1
			{Op: vm.OpLoad, Name: "i", Line: 3},
			{Op: vm.OpConst, Val: 1, Line: 3},
			{Op: vm.OpAdd, Line: 3},
			{Op: vm.OpStore, Name: "i", Line: 3},
			{Op: vm.OpJump, Target: 2, Line: 4},
		}},
	}}
}

func waitEvent(t *testing.T, s *Session, typ EventType, timeout time.Duration) Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatalf("event channel closed while waiting for %s", typ)
			}
			if ev.Type == typ {
				return ev
			}
		case <-deadline:
			t.Fatalf("timeout waiting for event %s", typ)
		}
	}
}

func TestBreakpointStopInspectContinue(t *testing.T) {
	svc := NewService()
	v := vm.New(loopProgram(), nil)
	svc.RegisterTarget("app", v)

	sess, err := svc.Attach(AttachRequest{Target: "app", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	bps, err := sess.SetBreakpoints([]Breakpoint{{Function: "main", Line: 3}})
	if err != nil {
		t.Fatal(err)
	}
	if len(bps) != 1 || bps[0].ID == 0 {
		t.Fatalf("bad breakpoints: %+v", bps)
	}

	done := make(chan struct{})
	go func() { v.Run(); close(done) }()

	ev := waitEvent(t, sess, EventStopped, 2*time.Second)
	if ev.Reason != ReasonBreakpoint {
		t.Fatalf("reason = %s", ev.Reason)
	}
	taskID := ev.TaskID

	st, err := sess.TaskState(taskID)
	if err != nil || st != "paused" {
		t.Fatalf("task state = %q, %v", st, err)
	}

	frames, err := sess.StackTrace(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || frames[0].Function != "main" || frames[0].Line != 3 {
		t.Fatalf("frames = %+v", frames)
	}

	vars, err := sess.Variables(frames[0].ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, vb := range vars {
		if vb.Name == "i" {
			found = true
		}
	}
	if !found {
		t.Fatalf("variable i not found in %+v", vars)
	}

	val, err := sess.Evaluate(context.Background(), frames[0].ID, "i + 10")
	if err != nil {
		t.Fatal(err)
	}
	if val != 10.0 {
		t.Fatalf("i+10 = %v, want 10", val)
	}

	if err := sess.Continue(taskID); err != nil {
		t.Fatal(err)
	}
	// Line 3 executes 3 times total (i=0,1,2); two more stops remain.
	for i := 0; i < 2; i++ {
		ev = waitEvent(t, sess, EventStopped, 2*time.Second)
		sess.Continue(ev.TaskID)
	}
	<-done
	waitEvent(t, sess, EventTaskEnd, 2*time.Second)
	sess.Terminate()
	if _, ok := <-sess.Events(); ok {
		t.Fatal("no events expected after terminate")
	}
}
