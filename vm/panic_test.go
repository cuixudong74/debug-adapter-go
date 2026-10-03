package vm

import (
	"strings"
	"testing"
)

// panicHook panics inside OnStep to simulate a crash during execution.
type panicHook struct {
	panicked  bool
	excErr    error
	excCalled bool
}

func (h *panicHook) OnTaskStart(t *Task) {}
func (h *panicHook) OnStep(t *Task) {
	if !h.panicked {
		h.panicked = true
		panic("simulated crash")
	}
}
func (h *panicHook) OnException(t *Task, err error) {
	h.excCalled = true
	h.excErr = err
}
func (h *panicHook) OnTaskEnd(t *Task) {}

func TestPanicRecoveredAsException(t *testing.T) {
	prog := &Program{Main: "main", Funcs: map[string]*Function{
		"main": {Name: "main", Code: []Instr{
			{Op: OpConst, Val: 1, Line: 1},
			{Op: OpStore, Name: "x", Line: 1},
		}},
	}}
	h := &panicHook{}
	v := New(prog, h)
	v.Run() // must not crash the process
	if !h.excCalled {
		t.Fatal("panic must be reported via OnException")
	}
	if !strings.Contains(h.excErr.Error(), "simulated crash") {
		t.Fatalf("exception = %v", h.excErr)
	}
	if v.Tasks()[0].Err() == nil {
		t.Fatal("task error must be recorded after a crash")
	}
}
