package debug

import (
	"testing"
	"time"

	"github.com/cuixudong74/debug-adapter-go/vm"
)

// callProgram: main line1: x=1; line2: y=add(x,2); line3: z=y+1.
// add(a,b) line 10: r=a+b; line 11: return r.
func callProgram() *vm.Program {
	return &vm.Program{Main: "main", Funcs: map[string]*vm.Function{
		"main": {Name: "main", Code: []vm.Instr{
			{Op: vm.OpConst, Val: 1, Line: 1},
			{Op: vm.OpStore, Name: "x", Line: 1},
			{Op: vm.OpLoad, Name: "x", Line: 2},
			{Op: vm.OpConst, Val: 2, Line: 2},
			{Op: vm.OpCall, Name: "add", Val: 2, Line: 2},
			{Op: vm.OpStore, Name: "y", Line: 2},
			{Op: vm.OpLoad, Name: "y", Line: 3},
			{Op: vm.OpConst, Val: 1, Line: 3},
			{Op: vm.OpAdd, Line: 3},
			{Op: vm.OpStore, Name: "z", Line: 3},
		}},
		"add": {Name: "add", Params: []string{"a", "b"}, Code: []vm.Instr{
			{Op: vm.OpLoad, Name: "a", Line: 10},
			{Op: vm.OpLoad, Name: "b", Line: 10},
			{Op: vm.OpAdd, Line: 10},
			{Op: vm.OpStore, Name: "r", Line: 10},
			{Op: vm.OpLoad, Name: "r", Line: 11},
			{Op: vm.OpReturn, Line: 11},
		}},
	}}
}

func TestStepIntoCrossesCallBoundary(t *testing.T) {
	svc := NewService()
	v := vm.New(callProgram(), nil)
	svc.RegisterTarget("app", v)
	sess, _ := svc.Attach(AttachRequest{Target: "app"})
	if _, err := sess.SetBreakpoints([]Breakpoint{{Function: "main", Line: 2}}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { v.Run(); close(done) }()

	ev := waitEvent(t, sess, EventStopped, 2*time.Second) // at line 2
	if err := sess.StepInto(ev.TaskID); err != nil {
		t.Fatal(err)
	}
	ev = waitEvent(t, sess, EventStopped, 2*time.Second) // inside add, line 10
	frames, err := sess.StackTrace(ev.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 || frames[0].Function != "add" || frames[0].Line != 10 {
		t.Fatalf("expected to step into add at line 10, got %+v", frames)
	}
	// Step over inside add: line 10 -> line 11 (same depth).
	if err := sess.StepOver(ev.TaskID); err != nil {
		t.Fatal(err)
	}
	ev = waitEvent(t, sess, EventStopped, 2*time.Second)
	frames, _ = sess.StackTrace(ev.TaskID)
	if len(frames) != 2 || frames[0].Line != 11 {
		t.Fatalf("expected line 11 in add, got %+v", frames)
	}
	sess.Continue(ev.TaskID)
	<-done
	sess.Terminate()
}

func TestStepOverSkipsCallee(t *testing.T) {
	svc := NewService()
	v := vm.New(callProgram(), nil)
	svc.RegisterTarget("app", v)
	sess, _ := svc.Attach(AttachRequest{Target: "app"})
	sess.SetBreakpoints([]Breakpoint{{Function: "main", Line: 2}})
	done := make(chan struct{})
	go func() { v.Run(); close(done) }()

	ev := waitEvent(t, sess, EventStopped, 2*time.Second) // at line 2
	if err := sess.StepOver(ev.TaskID); err != nil {
		t.Fatal(err)
	}
	ev = waitEvent(t, sess, EventStopped, 2*time.Second) // at line 3, callee skipped
	frames, err := sess.StackTrace(ev.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || frames[0].Function != "main" || frames[0].Line != 3 {
		t.Fatalf("expected main line 3 after step-over, got %+v", frames)
	}
	// The callee must have actually executed: y == 3 now.
	val, err := sess.Evaluate(testCtx(), frames[0].ID, "y")
	if err != nil {
		t.Fatal(err)
	}
	if val != 3.0 {
		t.Fatalf("y = %v, want 3 (callee must have run)", val)
	}
	sess.Continue(ev.TaskID)
	<-done
	sess.Terminate()
}
