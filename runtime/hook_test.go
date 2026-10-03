package runtime

import (
	"testing"
	"time"
)

// gateHook pauses every task on its first line, then resumes it.
type gateHook struct {
	firstLine chan struct{}
}

func (h *gateHook) BeforeLine(t *Task) {
	if t.Top().pc == 0 {
		close(h.firstLine)
		t.SetPaused(true)
		t.WaitWhilePaused()
	}
}
func (h *gateHook) OnThrow(t *Task, thrown *Value) bool { return false }
func (h *gateHook) OnTaskDone(t *Task, thrown *Value, fatal error) {}

func TestPauseGateBlocksAndResumes(t *testing.T) {
	p := NewProgram("g")
	p.Func("main", "m.go").Nop(1).Nop(2).Return(3)
	tgt := NewTarget(p)
	h := &gateHook{firstLine: make(chan struct{})}
	tgt.SetHook(h)
	main, err := tgt.RunMain("main")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.firstLine:
	case <-time.After(time.Second):
		t.Fatal("hook never called")
	}
	// task must be paused and not advance
	if !main.IsPaused() {
		t.Fatal("expected task paused")
	}
	line := main.Top().Line
	time.Sleep(20 * time.Millisecond)
	if main.Top().Line != line {
		t.Fatalf("task advanced while paused: line %d -> %d", line, main.Top().Line)
	}
	// resume
	main.SetPaused(false)
	tgt.Wait()
	if !main.IsDone() {
		t.Fatal("task should finish after resume")
	}
}

func TestBackgroundTaskUnaffectedByPause(t *testing.T) {
	p := NewProgram("g")
	p.Func("main", "m.go").Call(1, "worker").Return(2)
	p.Func("worker", "w.go").Nop(1).Nop(2).Return(3)
	tgt := NewTarget(p)
	// hook pauses only tasks named "main"
	tgt.SetHook(&nameHook{})
	if _, err := tgt.RunMain("main"); err != nil {
		t.Fatal(err)
	}
	tgt.Wait()
}

type nameHook struct{}

func (h *nameHook) BeforeLine(t *Task) {
	if t.Name == "main" && t.Top().pc == 0 {
		t.SetPaused(true)
		go func() {
			time.Sleep(50 * time.Millisecond)
			t.SetPaused(false)
		}()
		t.WaitWhilePaused()
	}
}
func (h *nameHook) OnThrow(t *Task, thrown *Value) bool { return false }
func (h *nameHook) OnTaskDone(t *Task, thrown *Value, fatal error) {}
