package runtime

import (
	"sync"
)

// Target is a running script program. It owns all tasks and the hook used by
// the debug service.
type Target struct {
	prog *Program

	hookMu sync.RWMutex
	hookVal Hook

	tasksMu sync.Mutex
	tasks   []*Task
	nextID  int

	main *Task

	stopMu sync.Mutex
	stop   bool

	finishOnce sync.Once
	finishMu   sync.Mutex
	finishCh   chan struct{}
	finishErr  *Value
	fatalErr   error

	yieldMu sync.RWMutex
	yield   func()
}

// NewTarget creates a target for the given program.
func NewTarget(prog *Program) *Target {
	return &Target{
		prog:     prog,
		finishCh: make(chan struct{}),
	}
}

// Program returns the underlying program.
func (t *Target) Program() *Program { return t.prog }

// SetYield installs a function called at the top of each line execution.
// It is intended for tests that need to slow execution deterministically.
func (t *Target) SetYield(f func()) {
	t.yieldMu.Lock()
	t.yield = f
	t.yieldMu.Unlock()
}

func (t *Target) yieldFunc() func() {
	t.yieldMu.RLock()
	defer t.yieldMu.RUnlock()
	return t.yield
}

// SetHook installs the debug hook. Passing nil detaches the debugger; tasks
// keep running.
func (t *Target) SetHook(h Hook) {
	t.hookMu.Lock()
	t.hookVal = h
	t.hookMu.Unlock()
}

func (t *Target) hook() Hook {
	t.hookMu.RLock()
	defer t.hookMu.RUnlock()
	return t.hookVal
}

// Spawn starts a new task running the given function.
func (t *Target) Spawn(name, funcName string, args ...*Value) (*Task, error) {
	fn, ok := t.prog.FuncDefOf(funcName)
	if !ok {
		return nil, errUndefinedFunc(funcName)
	}
	t.tasksMu.Lock()
	t.nextID++
	id := t.nextID
	tk := newTask(t, id, name)
	t.tasks = append(t.tasks, tk)
	t.tasksMu.Unlock()
	tk.run(fn, args)
	return tk, nil
}

// RunMain starts the main task.
func (t *Target) RunMain(funcName string, args ...*Value) (*Task, error) {
	tk, err := t.Spawn("main", funcName, args...)
	if err != nil {
		return nil, err
	}
	t.main = tk
	return tk, nil
}

// Main returns the main task (nil before RunMain).
func (t *Target) Main() *Task { return t.main }

// Tasks returns a snapshot of all tasks.
func (t *Target) Tasks() []*Task {
	t.tasksMu.Lock()
	defer t.tasksMu.Unlock()
	out := make([]*Task, len(t.tasks))
	copy(out, t.tasks)
	return out
}

// TaskByID returns the task with the given id.
func (t *Target) TaskByID(id int) (*Task, bool) {
	t.tasksMu.Lock()
	defer t.tasksMu.Unlock()
	for _, tk := range t.tasks {
		if tk.ID == id {
			return tk, true
		}
	}
	return nil, false
}

// Shutdown wakes every paused task and prevents further hook blocking.
func (t *Target) Shutdown() {
	t.stopMu.Lock()
	t.stop = true
	t.stopMu.Unlock()
	t.tasksMu.Lock()
	for _, tk := range t.tasks {
		tk.Abort()
	}
	t.tasksMu.Unlock()
}

func (t *Target) isShuttingDown() bool {
	t.stopMu.Lock()
	defer t.stopMu.Unlock()
	return t.stop
}

// Wait blocks until the main task finishes.
func (t *Target) Wait() {
	<-t.finishCh
}

// Outcome describes how a target ended.
type Outcome struct {
	Thrown *Value
	Fatal  error
}

// Outcome returns the terminal outcome (blocks until the main task ends).
func (t *Target) OutcomeResult() Outcome {
	<-t.finishCh
	t.finishMu.Lock()
	defer t.finishMu.Unlock()
	return Outcome{Thrown: t.finishErr, Fatal: t.fatalErr}
}

func (t *Target) taskFinished(tk *Task, thrown *Value, fatal error) {
	if tk != t.main {
		// Background tasks do not terminate the target.
		if h := t.hook(); h != nil {
			h.OnTaskDone(tk, thrown, fatal)
		}
		return
	}
	t.finishOnce.Do(func() {
		t.finishMu.Lock()
		t.finishErr = thrown
		t.fatalErr = fatal
		t.finishMu.Unlock()
		close(t.finishCh)
		t.Shutdown()
		if h := t.hook(); h != nil {
			h.OnTaskDone(tk, thrown, fatal)
		}
	})
}

// ResumeAll unpauses every paused task.
func (t *Target) ResumeAll() {
	t.tasksMu.Lock()
	defer t.tasksMu.Unlock()
	for _, tk := range t.tasks {
		tk.SetPaused(false)
	}
}

func errUndefinedFunc(name string) error {
	return &undefinedFuncError{name: name}
}

type undefinedFuncError struct{ name string }

func (e *undefinedFuncError) Error() string { return "undefined function: " + e.name }
