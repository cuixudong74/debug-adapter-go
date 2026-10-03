package debug

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cuixudong74/debug-adapter-go/vm"
)

// TestStepDoesNotStopSiblingTasks: stepping one task must not pause
// other execution units of the same target, and a paused task makes no
// progress while siblings keep running.
func TestStepDoesNotStopSiblingTasks(t *testing.T) {
	// main: spawns worker, then loops on line 5. worker: line 20 loop.
	mainCode := []vm.Instr{
		{Op: vm.OpSpawn, Name: "worker", Line: 1},
		{Op: vm.OpConst, Val: 0, Line: 2},
		{Op: vm.OpStore, Name: "m", Line: 2},
		// line 3: while m < 4
		{Op: vm.OpLoad, Name: "m", Line: 3},
		{Op: vm.OpConst, Val: 4, Line: 3},
		{Op: vm.OpLt, Line: 3},
		{Op: vm.OpJumpIfFalse, Target: 12, Line: 3},
		// line 4: m = m + 1
		{Op: vm.OpLoad, Name: "m", Line: 4},
		{Op: vm.OpConst, Val: 1, Line: 4},
		{Op: vm.OpAdd, Line: 4},
		{Op: vm.OpStore, Name: "m", Line: 4},
		{Op: vm.OpJump, Target: 3, Line: 5},
	}
	workerCode := []vm.Instr{
		{Op: vm.OpConst, Val: 0, Line: 20},
		{Op: vm.OpStore, Name: "w", Line: 20},
		{Op: vm.OpLoad, Name: "w", Line: 21},
		{Op: vm.OpConst, Val: 100, Line: 21},
		{Op: vm.OpLt, Line: 21},
		{Op: vm.OpJumpIfFalse, Target: 11, Line: 21},
		{Op: vm.OpLoad, Name: "w", Line: 22},
		{Op: vm.OpConst, Val: 1, Line: 22},
		{Op: vm.OpAdd, Line: 22},
		{Op: vm.OpStore, Name: "w", Line: 22},
		{Op: vm.OpJump, Target: 2, Line: 23},
	}
	prog := &vm.Program{Main: "main", Funcs: map[string]*vm.Function{
		"main":   {Name: "main", Code: mainCode},
		"worker": {Name: "worker", Code: workerCode},
	}}
	svc := NewService()
	v := vm.New(prog, nil)
	svc.RegisterTarget("app", v)
	sess, _ := svc.Attach(AttachRequest{Target: "app"})
	defer sess.Terminate()
	// Breakpoint only in main, line 4.
	sess.SetBreakpoints([]Breakpoint{{Function: "main", Line: 4}})

	done := make(chan struct{})
	go func() { v.Run(); close(done) }()

	ev := waitEvent(t, sess, EventStopped, 2*time.Second)
	mainID := ev.TaskID

	// While main is paused, the worker must keep running and finish.
	workerDone := false
	deadline := time.After(2 * time.Second)
	for !workerDone {
		select {
		case e := <-sess.Events():
			if e.Type == EventTaskEnd && e.TaskID != mainID {
				workerDone = true
			}
			if e.Type == EventStopped && e.TaskID != mainID {
				t.Fatalf("worker task %d was stopped; only main may stop", e.TaskID)
			}
		case <-deadline:
			t.Fatal("worker did not finish while main was paused")
		}
	}
	// Main is still paused at the same spot.
	st, _ := sess.TaskState(mainID)
	if st != "paused" {
		t.Fatalf("main state = %q, want paused", st)
	}
	// Step main through its loop; no other task is affected.
	for i := 0; i < 3; i++ {
		if err := sess.StepOver(mainID); err != nil {
			t.Fatal(err)
		}
		ev = waitEvent(t, sess, EventStopped, 2*time.Second)
		if ev.TaskID != mainID {
			t.Fatalf("step stopped task %d, want %d", ev.TaskID, mainID)
		}
	}
	sess.Continue(mainID)
	// Drain any further breakpoint hits until main finishes.
	for {
		select {
		case <-done:
			return
		case e := <-sess.Events():
			if e.Type == EventStopped {
				sess.Continue(e.TaskID)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("main did not finish")
		}
	}
}

// TestNestedVariablesLazy: nested structures are rendered shallowly
// and drilled into via paths, without side effects.
func TestNestedVariablesLazy(t *testing.T) {
	prog := &vm.Program{Main: "main", Funcs: map[string]*vm.Function{
		"main": {Name: "main", Code: []vm.Instr{
			{Op: vm.OpConst, Val: map[string]any{
				"user": map[string]any{"name": "ada", "tags": []any{"x", "y"}},
				"n":    7,
			}, Line: 1},
			{Op: vm.OpStore, Name: "cfg", Line: 1},
			{Op: vm.OpConst, Val: 1, Line: 2},
			{Op: vm.OpStore, Name: "k", Line: 2},
		}},
	}}
	svc := NewService()
	v := vm.New(prog, nil)
	svc.RegisterTarget("app", v)
	sess, _ := svc.Attach(AttachRequest{Target: "app", Role: "admin"})
	defer sess.Terminate()
	sess.SetBreakpoints([]Breakpoint{{Function: "main", Line: 2}})

	done := make(chan struct{})
	go func() { v.Run(); close(done) }()
	ev := waitEvent(t, sess, EventStopped, 2*time.Second)
	frames, _ := sess.StackTrace(ev.TaskID)

	vars, err := sess.Variables(frames[0].ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	var cfgVar *Variable
	for i := range vars {
		if vars[i].Name == "cfg" {
			cfgVar = &vars[i]
		}
	}
	if cfgVar == nil || !cfgVar.HasChildren || cfgVar.Value != "map[2]" {
		t.Fatalf("cfg rendered = %+v, want shallow map[2] with children", cfgVar)
	}
	// Drill into cfg.user.tags lazily.
	users, err := sess.Variables(frames[0].ID, []string{"cfg", "user"})
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 { // name, tags
		t.Fatalf("cfg.user children = %+v", users)
	}
	tags, err := sess.Variables(frames[0].ID, []string{"cfg", "user", "tags"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 || tags[0].Value != `"x"` {
		t.Fatalf("tags = %+v", tags)
	}
	// Evaluate a nested access expression.
	val, err := sess.Evaluate(testCtx(), frames[0].ID, `cfg.user.name`)
	if err != nil {
		t.Fatal(err)
	}
	if val != "ada" {
		t.Fatalf("cfg.user.name = %v", val)
	}
	sess.Continue(ev.TaskID)
	<-done
}

// TestProtocolBackwardCompat: an old client payload lacking the new
// optional fields still attaches and works with safe defaults.
func TestProtocolBackwardCompat(t *testing.T) {
	svc := NewService()
	v := vm.New(crashProgram(), nil)
	svc.RegisterTarget("app", v)

	// Old client: only knows the "target" field.
	var req AttachRequest
	if err := json.Unmarshal([]byte(`{"target":"app"}`), &req); err != nil {
		t.Fatal(err)
	}
	sess, err := svc.Attach(req)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Terminate()
	if sess.Role() != "viewer" {
		t.Fatalf("default role = %q, want viewer (least privilege)", sess.Role())
	}
	// ExceptionBreak defaults to true: the crash pauses for the old
	// client too.
	done := make(chan struct{})
	go func() { v.Run(); close(done) }()
	ev := waitEvent(t, sess, EventException, 2*time.Second)
	if st, _ := sess.TaskState(ev.TaskID); st != "paused" {
		t.Fatalf("state = %q, want paused", st)
	}
	sess.Continue(ev.TaskID)
	<-done

	// Event JSON round-trip keeps working when new fields are absent.
	data, err := json.Marshal(Event{Seq: 1, Type: EventStopped, TaskID: 3, Reason: ReasonStep})
	if err != nil {
		t.Fatal(err)
	}
	var back Event
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Reason != ReasonStep || back.Dropped != 0 {
		t.Fatalf("round-trip = %+v", back)
	}
}
