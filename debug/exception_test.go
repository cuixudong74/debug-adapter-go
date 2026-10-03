package debug

import (
	"strings"
	"testing"
	"time"

	"github.com/cuixudong74/debug-adapter-go/vm"
)

// crashProgram: main calls boom; boom sets secret_token then throws.
func crashProgram() *vm.Program {
	return &vm.Program{Main: "main", Funcs: map[string]*vm.Function{
		"main": {Name: "main", Code: []vm.Instr{
			{Op: vm.OpCall, Name: "boom", Val: 0, Line: 1},
			{Op: vm.OpStore, Name: "r", Line: 1},
		}},
		"boom": {Name: "boom", Code: []vm.Instr{
			{Op: vm.OpConst, Val: "tok-123", Line: 5},
			{Op: vm.OpStore, Name: "secret_token", Line: 5},
			{Op: vm.OpConst, Val: 42, Line: 6},
			{Op: vm.OpStore, Name: "answer", Line: 6},
			{Op: vm.OpConst, Val: "kaboom", Line: 7},
			{Op: vm.OpThrow, Line: 7},
		}},
	}}
}

// TestExceptionCapturedAndPaused: an uncaught exception pauses the
// task with the scene preserved; after resume the task terminates and
// its state is distinguishable.
func TestExceptionCapturedAndPaused(t *testing.T) {
	svc := NewService()
	v := vm.New(crashProgram(), nil)
	svc.RegisterTarget("app", v)
	sess, _ := svc.Attach(AttachRequest{Target: "app", Role: "admin"})
	defer sess.Terminate()

	done := make(chan struct{})
	go func() { v.Run(); close(done) }()

	ev := waitEvent(t, sess, EventException, 2*time.Second)
	if !strings.Contains(ev.Message, "kaboom") {
		t.Fatalf("exception message = %q", ev.Message)
	}
	// Paused, not terminated, with the crash scene preserved.
	st, err := sess.TaskState(ev.TaskID)
	if err != nil || st != "paused" {
		t.Fatalf("state after crash = %q, %v; want paused", st, err)
	}
	frames, err := sess.StackTrace(ev.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 || frames[0].Function != "boom" || frames[0].Line != 7 {
		t.Fatalf("crash frames = %+v", frames)
	}
	// Variables at the crash point are inspectable.
	val, err := sess.Evaluate(testCtx(), frames[0].ID, "answer + 1")
	if err != nil {
		t.Fatal(err)
	}
	if val != 43.0 {
		t.Fatalf("answer+1 = %v", val)
	}
	// Resume: the task then terminates (exception is uncaught).
	sess.Continue(ev.TaskID)
	<-done
	waitEvent(t, sess, EventTaskEnd, 2*time.Second)
	st, err = sess.TaskState(ev.TaskID)
	if err != nil || st != "terminated" {
		t.Fatalf("state after resume = %q, %v; want terminated", st, err)
	}
	if v.Tasks()[0].Err() == nil {
		t.Fatal("task error must be preserved")
	}
}

// TestExceptionBreakDisabled: with ExceptionBreak=false the session is
// notified but the task is not paused.
func TestExceptionBreakDisabled(t *testing.T) {
	svc := NewService()
	v := vm.New(crashProgram(), nil)
	svc.RegisterTarget("app", v)
	off := false
	sess, _ := svc.Attach(AttachRequest{Target: "app", ExceptionBreak: &off})
	defer sess.Terminate()

	done := make(chan struct{})
	go func() { v.Run(); close(done) }()

	ev := waitEvent(t, sess, EventException, 2*time.Second)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("task should terminate without a pause")
	}
	st, _ := sess.TaskState(ev.TaskID)
	if st != "terminated" {
		t.Fatalf("state = %q, want terminated (no pause)", st)
	}
}

// TestSensitiveVariablePermission: viewer role cannot read sensitive
// variables; attempts are denied and audited. Admin can.
func TestSensitiveVariablePermission(t *testing.T) {
	svc := NewService()
	v := vm.New(crashProgram(), nil)
	svc.RegisterTarget("app", v)

	viewer, _ := svc.Attach(AttachRequest{Target: "app"}) // default role
	admin, _ := svc.Attach(AttachRequest{Target: "app", Role: "admin"})
	defer viewer.Terminate()
	defer admin.Terminate()

	done := make(chan struct{})
	go func() { v.Run(); close(done) }()

	// Both sessions pause at the exception.
	evV := waitEvent(t, viewer, EventException, 2*time.Second)
	evA := waitEvent(t, admin, EventException, 2*time.Second)

	framesV, _ := viewer.StackTrace(evV.TaskID)
	vars, err := viewer.Variables(framesV[0].ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, vb := range vars {
		if vb.Name == "secret_token" {
			if !vb.Sensitive || vb.Value != "<restricted>" {
				t.Fatalf("sensitive var not redacted for viewer: %+v", vb)
			}
		}
	}
	// Explicit read of a sensitive path: denied.
	if _, err := viewer.Variables(framesV[0].ID, []string{"secret_token"}); err == nil ||
		!strings.Contains(err.Error(), ErrPermissionDenied.Error()) {
		t.Fatalf("viewer sensitive read = %v, want permission denied", err)
	}
	// Evaluate mentioning a sensitive identifier: denied.
	if _, err := viewer.Evaluate(testCtx(), framesV[0].ID, "secret_token"); err == nil {
		t.Fatal("viewer evaluate of sensitive var must be denied")
	}
	// Non-sensitive evaluate still works for viewer.
	if _, err := viewer.Evaluate(testCtx(), framesV[0].ID, "answer"); err != nil {
		t.Fatalf("viewer evaluate of normal var: %v", err)
	}

	// Admin can read it.
	framesA, _ := admin.StackTrace(evA.TaskID)
	val, err := admin.Evaluate(testCtx(), framesA[0].ID, "secret_token")
	if err != nil {
		t.Fatalf("admin evaluate: %v", err)
	}
	if val != "tok-123" {
		t.Fatalf("admin secret_token = %v", val)
	}

	viewer.Continue(evV.TaskID)
	admin.Continue(evA.TaskID)
	<-done

	// Audit: the denied attempts are recorded with Allowed=false.
	var denied, allowed int
	for _, e := range svc.Audit() {
		if e.SessionID == viewer.ID() && !e.Allowed {
			denied++
		}
		if e.Allowed {
			allowed++
		}
	}
	if denied < 2 {
		t.Fatalf("audit denied entries = %d, want >= 2", denied)
	}
	if allowed == 0 {
		t.Fatal("audit should also record allowed reads")
	}
}
