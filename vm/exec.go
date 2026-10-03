package vm

import "fmt"

// Exception is an uncaught script error raised by OpThrow or by a
// runtime fault (division by zero, unknown variable, ...).
type Exception struct{ Msg string }

func (e *Exception) Error() string { return e.Msg }

// spawn starts fn as a new concurrent task.
func (v *VM) spawn(fn *Function, args []any) *Task {
	v.mu.Lock()
	t := &Task{ID: v.nextID, vm: v, done: make(chan struct{})}
	v.nextID++
	v.tasks[t.ID] = t
	hook := v.hook
	v.mu.Unlock()

	v.wg.Add(1)
	go v.runTask(t, fn, args)
	_ = hook
	return t
}

// runTask executes a task to completion. A panic in script execution
// is recovered and reported exactly like an uncaught Exception, so the
// debug hook always sees a preserved call stack.
func (v *VM) runTask(t *Task, fn *Function, args []any) {
	defer v.wg.Done()
	defer close(t.done)
	defer func() {
		if r := recover(); r != nil {
			t.err = fmt.Errorf("vm: panic: %v", r)
			if h := v.getHook(); h != nil {
				h.OnException(t, t.err)
			}
		}
		if h := v.getHook(); h != nil {
			h.OnTaskEnd(t)
		}
	}()

	if h := v.getHook(); h != nil {
		h.OnTaskStart(t)
	}
	err := v.exec(t, fn, args)
	if _, ok := err.(errReturn); ok {
		err = nil // top-level return is normal completion
	}
	if err != nil {
		t.err = err
		if h := v.getHook(); h != nil {
			h.OnException(t, err) // frames are still intact here
		}
	}
}

func (v *VM) getHook() Hook {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.hook
}

// exec runs fn in task t, propagating errors up the call stack.
func (v *VM) exec(t *Task, fn *Function, args []any) (err error) {
	f := &Frame{Fn: fn, Locals: map[string]any{}}
	for i, p := range fn.Params {
		if i < len(args) {
			f.Locals[p] = args[i]
		}
	}
	t.Frames = append(t.Frames, f)
	defer func() {
		// On a propagated error keep the frames for post-mortem
		// inspection; the hook is invoked before unwinding by runTask.
		// A return value (errReturn) is normal completion.
		if err == nil || isReturn(err) {
			t.Frames = t.Frames[:len(t.Frames)-1]
		}
	}()

	for f.PC < len(fn.Code) {
		if h := v.getHook(); h != nil {
			h.OnStep(t) // may block to pause the task
		}
		ins := fn.Code[f.PC]
		f.PC++
		if err = v.step(t, f, ins); err != nil {
			f.PC-- // point at the failing instruction in the preserved scene
			return err
		}
	}
	return nil
}

// step executes a single instruction (PC already advanced).
func (v *VM) step(t *Task, f *Frame, ins Instr) error {
	push := func(x any) { f.stack = append(f.stack, x) }
	pop := func() any {
		if len(f.stack) == 0 {
			return nil
		}
		x := f.stack[len(f.stack)-1]
		f.stack = f.stack[:len(f.stack)-1]
		return x
	}

	switch ins.Op {
	case OpConst:
		push(ins.Val)
	case OpLoad:
		x, ok := f.Locals[ins.Name]
		if !ok {
			return &Exception{Msg: fmt.Sprintf("undefined variable %q", ins.Name)}
		}
		push(x)
	case OpStore:
		f.Locals[ins.Name] = pop()
	case OpAdd, OpSub, OpMul, OpDiv, OpEq, OpLt:
		b, a := pop(), pop()
		r, err := arith(ins.Op, a, b)
		if err != nil {
			return err
		}
		push(r)
	case OpNot:
		push(!truthy(pop()))
	case OpJump:
		f.PC = ins.Target
	case OpJumpIfFalse:
		if !truthy(pop()) {
			f.PC = ins.Target
		}
	case OpCall:
		n, _ := ins.Val.(int)
		callee := v.prog.Funcs[ins.Name]
		if callee == nil {
			return &Exception{Msg: fmt.Sprintf("unknown function %q", ins.Name)}
		}
		args := make([]any, n)
		for i := n - 1; i >= 0; i-- {
			args[i] = pop()
		}
		ret, err := v.call(t, callee, args)
		if err != nil {
			return err
		}
		push(ret)
	case OpSpawn:
		callee := v.prog.Funcs[ins.Name]
		if callee == nil {
			return &Exception{Msg: fmt.Sprintf("unknown function %q", ins.Name)}
		}
		v.spawn(callee, nil)
	case OpReturn:
		return errReturn{pop()}
	case OpThrow:
		return &Exception{Msg: fmt.Sprintf("uncaught exception: %v", pop())}
	default:
		return &Exception{Msg: fmt.Sprintf("bad opcode %d", ins.Op)}
	}
	return nil
}

// errReturn carries a return value out of exec.
type errReturn struct{ val any }

func (errReturn) Error() string { return "return" }

func isReturn(err error) bool {
	_, ok := err.(errReturn)
	return ok
}

// call invokes callee and returns its result.
func (v *VM) call(t *Task, callee *Function, args []any) (any, error) {
	err := v.exec(t, callee, args)
	if r, ok := err.(errReturn); ok {
		return r.val, nil
	}
	return nil, err
}

func arith(op Op, a, b any) (any, error) {
	if op == OpEq {
		return fmt.Sprint(a) == fmt.Sprint(b) && sameKind(a, b), nil
	}
	fa, oka := toFloat(a)
	fb, okb := toFloat(b)
	if !oka || !okb {
		return nil, &Exception{Msg: fmt.Sprintf("arithmetic on non-numbers %v, %v", a, b)}
	}
	switch op {
	case OpAdd:
		return fa + fb, nil
	case OpSub:
		return fa - fb, nil
	case OpMul:
		return fa * fb, nil
	case OpDiv:
		if fb == 0 {
			return nil, &Exception{Msg: "division by zero"}
		}
		return fa / fb, nil
	case OpLt:
		return fa < fb, nil
	}
	return nil, &Exception{Msg: "bad arithmetic op"}
}

func sameKind(a, b any) bool {
	_, aNum := toFloat(a)
	_, bNum := toFloat(b)
	return aNum == bNum
}

func toFloat(x any) (float64, bool) {
	switch n := x.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func truthy(x any) bool {
	switch v := x.(type) {
	case nil:
		return false
	case bool:
		return v
	}
	if f, ok := toFloat(x); ok {
		return f != 0
	}
	return true
}
