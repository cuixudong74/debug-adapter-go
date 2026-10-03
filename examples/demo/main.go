// Command demo shows how to drive the in-process debug service: launch a
// script target, set breakpoints, step, inspect variables and evaluate
// expressions — all without any external service.
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/cuixudong74/debug-adapter-go/debug"
	"github.com/cuixudong74/debug-adapter-go/runtime"
)

func main() {
	svc := debug.NewService()

	// Build a small script program.
	prog := runtime.NewProgram("demo")
	prog.Source("main.go", "line1\nline2\nline3\nline4\n")
	prog.Func("main", "main.go").
		Param("argc").
		Local("acc").
		Nop(1).
		Call(2, "add", runtime.IntValue(3), runtime.IntValue(4)).
		Nop(3).
		Return(4)
	prog.Func("add", "add.go").
		Param("a", "b").
		Local("sum").
		Nop(1).
		Nop(2).
		Return(3)
	prog.Global("version", runtime.StringValue("1.0"))

	user := debug.User{
		ID: "demo-user", Name: "demo",
		Roles: []string{
			string(debug.ScopeControl),
			string(debug.ScopeReadVariables),
			string(debug.ScopeEvaluate),
			string(debug.ScopeReadSensitive),
		},
	}

	sess, err := svc.Launch(context.Background(), "demo-target", prog, user)
	if err != nil {
		panic(err)
	}

	// Set a breakpoint at main.go:2 (the call to add).
	bps, err := sess.SetBreakpoints(context.Background(), "main.go", []debug.BreakpointSpec{
		{Line: 2},
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("set %d breakpoint(s)\n", len(bps))

	// Wait for the breakpoint hit.
	ev, err := sess.Events().Next(waitCtx())
	if err != nil {
		panic(err)
	}
	fmt.Printf("stopped: reason=%s task=%d\n", ev.Reason, ev.TaskID)

	// Inspect the call stack.
	frames, err := sess.Stack(context.Background(), ev.TaskID)
	if err != nil {
		panic(err)
	}
	for _, f := range frames {
		fmt.Printf("  frame %d: %s:%d\n", f.ID, f.Name, f.Line)
	}

	// Evaluate an expression (side-effect free).
	v, err := sess.Evaluate(context.Background(), "1 + 2 * 3", frames[0].ID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("evaluate 1 + 2 * 3 = %s\n", v.Value)

	// Step into the call.
	if err := sess.StepIn(context.Background(), ev.TaskID); err != nil {
		panic(err)
	}
	for {
		ev, err = sess.Events().Next(waitCtx())
		if err != nil {
			panic(err)
		}
		if ev.Kind == debug.EvStopped {
			break
		}
	}
	fmt.Printf("stopped: reason=%s task=%d\n", ev.Reason, ev.TaskID)
	frames, _ = sess.Stack(context.Background(), ev.TaskID)
	fmt.Printf("  now in frame: %s:%d\n", frames[0].Name, frames[0].Line)

	// Continue to termination.
	if err := sess.Continue(context.Background()); err != nil {
		panic(err)
	}
	for {
		ev, err = sess.Events().Next(waitCtx())
		if err != nil {
			panic(err)
		}
		if ev.Kind == debug.EvTerminated {
			fmt.Printf("terminated: fatal=%v\n", ev.Fatal)
			break
		}
	}
	fmt.Println("done")
}

func waitCtx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_ = cancel
	return ctx
}
