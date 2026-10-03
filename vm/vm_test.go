package vm

import (
	"strings"
	"testing"
)

// counter records hook invocations.
type counter struct{ steps int }

func (c *counter) OnTaskStart(t *Task)            {}
func (c *counter) OnStep(t *Task)                 { c.steps++ }
func (c *counter) OnException(t *Task, err error) {}
func (c *counter) OnTaskEnd(t *Task)              {}

func TestRunArithmetic(t *testing.T) {
	// main: x = 1 + 2; y = x * 3
	prog := &Program{Main: "main", Funcs: map[string]*Function{
		"main": {Name: "main", Code: []Instr{
			{Op: OpConst, Val: 1, Line: 1},
			{Op: OpConst, Val: 2, Line: 1},
			{Op: OpAdd, Line: 1},
			{Op: OpStore, Name: "x", Line: 1},
			{Op: OpLoad, Name: "x", Line: 2},
			{Op: OpConst, Val: 3, Line: 2},
			{Op: OpMul, Line: 2},
			{Op: OpStore, Name: "y", Line: 2},
		}},
	}}
	h := &counter{}
	v := New(prog, h)
	v.Run()
	if h.steps != 8 {
		t.Fatalf("steps = %d, want 8", h.steps)
	}
	task := v.Tasks()[0]
	if !task.Done() {
		t.Fatal("task should be done")
	}
	if got := task.Frames == nil || len(task.Frames) != 0; got {
		t.Fatal("frames should be unwound after normal completion")
	}
}

func TestCallAndSpawn(t *testing.T) {
	prog := &Program{Main: "main", Funcs: map[string]*Function{
		"main": {Name: "main", Code: []Instr{
			{Op: OpConst, Val: 5, Line: 1},
			{Op: OpCall, Name: "dbl", Val: 1, Line: 1},
			{Op: OpStore, Name: "r", Line: 1},
			{Op: OpSpawn, Name: "worker", Line: 2},
		}},
		"dbl": {Name: "dbl", Params: []string{"n"}, Code: []Instr{
			{Op: OpLoad, Name: "n", Line: 10},
			{Op: OpConst, Val: 2, Line: 10},
			{Op: OpMul, Line: 10},
			{Op: OpReturn, Line: 11},
		}},
		"worker": {Name: "worker", Code: []Instr{
			{Op: OpConst, Val: 1, Line: 20},
			{Op: OpStore, Name: "w", Line: 20},
		}},
	}}
	v := New(prog, nil)
	v.Run()
	if len(v.Tasks()) != 2 {
		t.Fatalf("tasks = %d, want 2", len(v.Tasks()))
	}
	for _, task := range v.Tasks() {
		if task.Err() != nil {
			t.Fatalf("task %d err: %v", task.ID, task.Err())
		}
	}
}

func TestUncaughtExceptionPreservesFrames(t *testing.T) {
	prog := &Program{Main: "main", Funcs: map[string]*Function{
		"main": {Name: "main", Code: []Instr{
			{Op: OpCall, Name: "boom", Val: 0, Line: 1},
		}},
		"boom": {Name: "boom", Code: []Instr{
			{Op: OpConst, Val: "kaboom", Line: 5},
			{Op: OpThrow, Line: 5},
		}},
	}}
	var gotErr error
	var framesAtException int
	h := &counter{}
	v := New(prog, hookFunc{
		onException: func(t *Task, err error) {
			gotErr = err
			framesAtException = len(t.Frames)
		},
	})
	_ = h
	v.Run()
	if gotErr == nil || !strings.Contains(gotErr.Error(), "kaboom") {
		t.Fatalf("err = %v", gotErr)
	}
	if framesAtException != 2 {
		t.Fatalf("frames at exception = %d, want 2", framesAtException)
	}
	if v.Tasks()[0].Err() == nil {
		t.Fatal("task.Err should be set")
	}
}

type hookFunc struct {
	onStep      func(t *Task)
	onException func(t *Task, err error)
}

func (h hookFunc) OnTaskStart(t *Task) {}
func (h hookFunc) OnStep(t *Task) {
	if h.onStep != nil {
		h.onStep(t)
	}
}
func (h hookFunc) OnException(t *Task, err error) {
	if h.onException != nil {
		h.onException(t, err)
	}
}
func (h hookFunc) OnTaskEnd(t *Task) {}
