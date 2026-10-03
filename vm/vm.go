// Package vm implements a tiny in-process script runtime (stack-based VM)
// that the debug service attaches to. Programs are built directly from
// instructions; there is no text parser by design.
package vm

import "sync"

// Op is a VM instruction opcode.
type Op int

const (
	OpConst Op = iota // push Val
	OpLoad            // push local Name
	OpStore           // pop into local Name
	OpAdd             // pop b, a; push a+b
	OpSub
	OpMul
	OpDiv
	OpEq
	OpLt
	OpNot
	OpJump        // pc = Target
	OpJumpIfFalse // pop cond; if false pc = Target
	OpCall        // call function Name with Val.(int) args
	OpSpawn       // start function Name as a new concurrent task
	OpReturn      // return top of stack (or nil) to caller
	OpThrow       // pop value and raise it as an exception
)

// Instr is a single instruction with its source line.
type Instr struct {
	Op     Op
	Name   string // variable or function name
	Val    any    // constant value (OpConst) or arg count (OpCall)
	Target int    // jump target (OpJump / OpJumpIfFalse)
	Line   int
}

// Function is a named sequence of instructions.
type Function struct {
	Name   string
	Params []string
	Code   []Instr
}

// Program is a set of functions; Main names the entry function.
type Program struct {
	Funcs map[string]*Function
	Main  string
}

// Frame is one activation record on a task's call stack.
type Frame struct {
	Fn     *Function
	PC     int
	Locals map[string]any
	stack  []any
}

// Task is a single execution unit (goroutine) with its own call stack.
// Frames are only safe to inspect while the task is paused inside a
// hook callback (the VM guarantees no progress during hook calls).
type Task struct {
	ID     int
	Frames []*Frame

	vm   *VM
	done chan struct{}
	err  error
}

// Hook receives execution callbacks. Implementations may block inside
// OnStep / OnException to pause the task; the VM guarantees the task
// makes no progress while a hook call is blocked.
type Hook interface {
	OnTaskStart(t *Task)
	OnStep(t *Task)
	OnException(t *Task, err error) // uncaught; frames still intact
	OnTaskEnd(t *Task)
}

// VM runs a Program and owns all of its tasks.
type VM struct {
	prog   *Program
	hook   Hook
	mu     sync.Mutex
	tasks  map[int]*Task
	nextID int
	wg     sync.WaitGroup
}

// New creates a VM for p. h may be nil (no debugging).
func New(p *Program, h Hook) *VM {
	return &VM{prog: p, hook: h, tasks: make(map[int]*Task), nextID: 1}
}

// SetHook installs (or replaces) the execution hook. It must be set
// before Run to observe the whole execution.
func (v *VM) SetHook(h Hook) {
	v.mu.Lock()
	v.hook = h
	v.mu.Unlock()
}

// Run executes the main function and blocks until every task
// (including spawned ones) has finished.
func (v *VM) Run() {
	main := v.prog.Funcs[v.prog.Main]
	if main == nil {
		return
	}
	v.spawn(main, nil)
	v.wg.Wait()
}

// Tasks returns a snapshot of all known tasks.
func (v *VM) Tasks() []*Task {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]*Task, 0, len(v.tasks))
	for _, t := range v.tasks {
		out = append(out, t)
	}
	return out
}

// TaskByID returns the task with the given ID, or nil.
func (v *VM) TaskByID(id int) *Task {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.tasks[id]
}

// Done reports whether the task has finished.
func (t *Task) Done() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// Err returns the uncaught error that ended the task, if any.
func (t *Task) Err() error {
	<-t.done
	return t.err
}

// CurrentLine returns the source line of the top frame's next instruction.
func (t *Task) CurrentLine() int {
	if len(t.Frames) == 0 {
		return 0
	}
	f := t.Frames[len(t.Frames)-1]
	if f.PC < 0 || f.PC >= len(f.Fn.Code) {
		return 0
	}
	return f.Fn.Code[f.PC].Line
}

// CurrentFunction returns the name of the top frame's function.
func (t *Task) CurrentFunction() string {
	if len(t.Frames) == 0 {
		return ""
	}
	return t.Frames[len(t.Frames)-1].Fn.Name
}

// Depth returns the current call depth (number of frames).
func (t *Task) Depth() int { return len(t.Frames) }

// VM returns the VM that owns the task.
func (t *Task) VM() *VM { return t.vm }

// AtLineStart reports whether the top frame's next instruction is the
// first instruction of its source line. Breakpoints and statement
// stepping use this so one line produces exactly one stop.
func (t *Task) AtLineStart() bool {
	if len(t.Frames) == 0 {
		return false
	}
	f := t.Frames[len(t.Frames)-1]
	if f.PC <= 0 || f.PC >= len(f.Fn.Code) {
		return f.PC == 0
	}
	return f.Fn.Code[f.PC-1].Line != f.Fn.Code[f.PC].Line
}
