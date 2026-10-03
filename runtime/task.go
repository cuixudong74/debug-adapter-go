package runtime

import (
	"fmt"
	"sync"
)

// Hook is the debug service's interface into the runtime. All methods are
// invoked from the task's own goroutine. BeforeLine may block (pause); the
// runtime guarantees that while blocked the task makes no progress.
type Hook interface {
	// BeforeLine is called before the operation on f.Line executes.
	// It may block until the task is resumed.
	BeforeLine(t *Task)
	// OnThrow is called when the task raises a script exception. Returning
	// true means the debugger paused the task at the throw site; after
	// resume the task terminates with the exception. Returning false lets
	// the task die immediately.
	OnThrow(t *Task, thrown *Value) (paused bool)
	// OnTaskDone is called when a task finishes (normally, via uncaught
	// exception, or via fatal crash). fatal is non-nil for crashes.
	OnTaskDone(t *Task, thrown *Value, fatal error)
}

// Frame is a single call frame.
type Frame struct {
	Func   *FuncDef
	File   string
	Line   int
	Locals map[string]*Value
	pc     int
	loops  map[int]int
}

// Task is an independent execution unit (goroutine).
type Task struct {
	ID   int
	Name string

	target *Target

	frameMu sync.Mutex
	frames  []*Frame

	// pause gate
	gateMu   sync.Mutex
	paused   bool
	aborting bool
	gate     *sync.Cond

	doneMu sync.Mutex
	done   bool
}

func newTask(target *Target, id int, name string) *Task {
	t := &Task{
		ID:     id,
		Name:   name,
		target: target,
	}
	t.gate = sync.NewCond(&t.gateMu)
	return t
}

// Frames returns a snapshot of the current call stack, deepest frame last.
func (t *Task) Frames() []*Frame {
	t.frameMu.Lock()
	defer t.frameMu.Unlock()
	out := make([]*Frame, len(t.frames))
	copy(out, t.frames)
	return out
}

// Depth returns the current frame depth.
func (t *Task) Depth() int {
	t.frameMu.Lock()
	defer t.frameMu.Unlock()
	return len(t.frames)
}

// Top returns the current top frame (nil when the task has no frame).
func (t *Task) Top() *Frame {
	t.frameMu.Lock()
	defer t.frameMu.Unlock()
	if len(t.frames) == 0 {
		return nil
	}
	return t.frames[len(t.frames)-1]
}

// IsDone reports whether the task has finished.
func (t *Task) IsDone() bool {
	t.doneMu.Lock()
	defer t.doneMu.Unlock()
	return t.done
}

func (t *Task) markDone() {
	t.doneMu.Lock()
	t.done = true
	t.doneMu.Unlock()
}

// ---- pause gate (used by the debug service via Target) ----

// SetPaused blocks the task in its line hook until Resume or Abort is called.
func (t *Task) SetPaused(v bool) {
	t.gateMu.Lock()
	t.paused = v
	if !v {
		t.gate.Broadcast()
	}
	t.gateMu.Unlock()
}

// IsPaused reports whether the task is currently stopped in its hook.
func (t *Task) IsPaused() bool {
	t.gateMu.Lock()
	defer t.gateMu.Unlock()
	return t.paused
}

// Abort wakes the task and prevents it from blocking again (target shutdown).
func (t *Task) Abort() {
	t.gateMu.Lock()
	t.aborting = true
	t.paused = false
	t.gate.Broadcast()
	t.gateMu.Unlock()
}

// WaitWhilePaused blocks the calling (task) goroutine while the task is marked
// paused. It returns when the task is resumed or aborted.
func (t *Task) WaitWhilePaused() {
	t.gateMu.Lock()
	for t.paused && !t.aborting {
		t.gate.Wait()
	}
	t.gateMu.Unlock()
}

// ---- frame management ----

func (t *Task) pushFrame(fn *FuncDef, args []*Value) *Frame {
	locals := map[string]*Value{}
	for i, name := range fn.Params {
		if i < len(args) && args[i] != nil {
			locals[name] = args[i]
		} else {
			locals[name] = NilValue()
		}
	}
	for _, name := range fn.Locals {
		if _, ok := locals[name]; !ok {
			locals[name] = NilValue()
		}
	}
	f := &Frame{Func: fn, File: fn.File, Line: fn.Ops[0].Line, Locals: locals, pc: 0, loops: map[int]int{}}
	t.frameMu.Lock()
	t.frames = append(t.frames, f)
	t.frameMu.Unlock()
	return f
}

func (t *Task) popFrame() {
	t.frameMu.Lock()
	if len(t.frames) > 0 {
		t.frames = t.frames[:len(t.frames)-1]
	}
	t.frameMu.Unlock()
}

// returnToCaller pops the current frame and advances the caller past the call
// site so execution does not re-enter the callee.
func (t *Task) returnToCaller() {
	t.frameMu.Lock()
	if len(t.frames) > 0 {
		t.frames = t.frames[:len(t.frames)-1]
	}
	if len(t.frames) > 0 {
		caller := t.frames[len(t.frames)-1]
		caller.pc++
		if caller.pc < len(caller.Func.Ops) {
			caller.Line = caller.Func.Ops[caller.pc].Line
		}
	}
	t.frameMu.Unlock()
}

// doJumpN implements bounded loops: jump to JumpLine at most JumpCount times.
func (t *Task) doJumpN(f *Frame, op Op) {
	if f.loops == nil {
		f.loops = map[int]int{}
	}
	loopPC := f.pc // key the counter by the loop op's own pc
	n := f.loops[loopPC]
	if n < op.JumpCount {
		for i := range f.Func.Ops {
			if f.Func.Ops[i].Line == op.JumpLine {
				f.pc = i
				f.Line = op.JumpLine
				f.loops[loopPC] = n + 1
				return
			}
		}
	}
	f.pc++
	if f.pc < len(f.Func.Ops) {
		f.Line = f.Func.Ops[f.pc].Line
	}
}

func (t *Task) run(fn *FuncDef, args []*Value) {
	t.pushFrame(fn, args)
	go t.loop()
}

func (t *Task) loop() {
	var thrown *Value
	var fatal error
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				fatal = e
			} else {
				fatal = fmt.Errorf("runtime panic: %v", r)
			}
		}
		t.markDone()
		t.target.taskFinished(t, thrown, fatal)
	}()

	for {
		if t.target.isShuttingDown() {
			return
		}
		f := t.Top()
		if f == nil {
			return
		}
		if f.pc >= len(f.Func.Ops) {
			// implicit return at end of function
			t.returnToCaller()
			continue
		}
		op := f.Func.Ops[f.pc]
		f.Line = op.Line

		// The debug hook may block here. While blocked the task does not
		// advance; no other task is affected.
		if h := t.target.hook(); h != nil {
			h.BeforeLine(t)
		}
		if t.target.isShuttingDown() || t.aborting {
			return
		}
		if y := t.target.yieldFunc(); y != nil {
			y()
		}

		switch op.Action {
		case ActNop:
			f.pc++
		case ActCall:
			callee, ok := t.target.prog.FuncDefOf(op.Call)
			if !ok {
				fatal = fmt.Errorf("call to undefined function %q", op.Call)
				return
			}
			t.pushFrame(callee, op.Args)
		case ActReturn:
			t.returnToCaller()
		case ActThrow:
			thrown = op.Thrown
			paused := false
			if h := t.target.hook(); h != nil {
				paused = h.OnThrow(t, thrown)
			}
			if paused {
				// The debugger paused the task at the throw site. After
				// resume the task terminates with the exception.
				t.WaitWhilePaused()
			}
			return
		case ActFatal:
			panic(fmt.Errorf("fatal runtime error: %s", op.Thrown.Display()))
		case ActJumpN:
			t.doJumpN(f, op)
		default:
			f.pc++
		}
	}
}
