package debug

import (
	"context"
	"testing"
	"time"

	"github.com/cuixudong74/debug-adapter-go/runtime"
)

func testProgram() *runtime.Program {
	p := runtime.NewProgram("test")
	p.Source("main.go", "line1\nline2\nline3\n")
	p.Func("main", "main.go").
		Param("argc").
		Local("acc").
		Nop(1).
		Call(2, "add", runtime.IntValue(3), runtime.IntValue(4)).
		Nop(3).
		Return(4)
	p.Func("add", "add.go").
		Param("a", "b").
		Local("sum").
		Nop(1).
		Nop(2).
		Return(3)
	p.Global("version", runtime.StringValue("1.0"))
	return p
}

func TestSmokeBreakpointAndContinue(t *testing.T) {
	svc := NewService()
	user := User{ID: "u1", Name: "alice", Roles: []string{string(ScopeControl), string(ScopeReadVariables), string(ScopeEvaluate), string(ScopeReadSensitive)}}
	svc.SetYield(func() { time.Sleep(2 * time.Millisecond) })
	sess, err := svc.Launch(context.Background(), "t1", testProgram(), user)
	if err != nil {
		t.Fatal(err)
	}
	// set breakpoint at main.go line 2 (the call)
	bps, err := sess.SetBreakpoints(context.Background(), "main.go", []BreakpointSpec{{Line: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if len(bps) != 1 || bps[0].ID != 1 {
		t.Fatalf("unexpected bps: %+v", bps)
	}
	// wait for stopped event
	ev, err := sess.Events().Next(waitCtx())
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != EvStopped || ev.Reason != ReasonBreakpoint {
		t.Fatalf("unexpected event: %+v", ev)
	}
	if sess.State() != statePaused {
		t.Fatalf("expected paused, got %s", sess.State())
	}
	// stack
	frames, err := sess.Stack(context.Background(), ev.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || frames[0].Name != "main" || frames[0].Line != 2 {
		t.Fatalf("unexpected frames: %+v", frames)
	}
	// evaluate
	v, err := sess.Evaluate(context.Background(), "argc", frames[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Value != "nil" { // argc is nil default
		t.Fatalf("expected argc=nil display, got %s", v.Value)
	}
	_ = v
	// continue
	if err := sess.Continue(context.Background()); err != nil {
		t.Fatal(err)
	}
	ev, err = sess.Events().Next(waitCtx())
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != EvContinued {
		t.Fatalf("expected continued, got %+v", ev)
	}
	// wait for terminated
	ev, err = sess.Events().Next(waitCtx())
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != EvTerminated {
		t.Fatalf("expected terminated, got %+v", ev)
	}
	if sess.State() != stateTerminated {
		t.Fatalf("expected terminated state, got %s", sess.State())
	}
}

func waitCtx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_ = cancel
	return ctx
}
