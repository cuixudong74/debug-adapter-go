package runtime

import "fmt"

// OpAction identifies what an operation does after the line hook fires.
type OpAction int

const (
	// ActNop advances to the next line.
	ActNop OpAction = iota
	// ActCall invokes the function named in Call.
	ActCall
	// ActReturn returns from the current function.
	ActReturn
	// ActThrow raises an uncaught script exception carrying Thrown.
	ActThrow
	// ActFatal panics with a Go error, simulating a runtime crash.
	ActFatal
	// ActJumpN jumps to JumpLine at most JumpCount times, then falls through.
	ActJumpN
)

// Op is a single source-level operation tied to a line.
type Op struct {
	Line   int
	Action OpAction
	Call   string
	Args   []*Value
	Thrown *Value
	// JumpLine is the target line for ActJumpN.
	JumpLine int
	// JumpCount is the maximum number of jumps for ActJumpN.
	JumpCount int
}

// FuncDef is a function definition: an ordered list of line operations.
type FuncDef struct {
	Name   string
	File   string
	Params []string
	Locals []string
	Ops    []Op
}

// Program is a script program: named functions plus source text for display.
type Program struct {
	name    string
	sources map[string]string
	funcs   map[string]*FuncDef
	order   []string
	globals map[string]*Value
}

// NewProgram creates an empty program.
func NewProgram(name string) *Program {
	return &Program{
		name:    name,
		sources: map[string]string{},
		funcs:   map[string]*FuncDef{},
		globals: map[string]*Value{},
	}
}

// Name returns the program name.
func (p *Program) Name() string { return p.name }

// Source registers source text for a file.
func (p *Program) Source(file, text string) *Program {
	p.sources[file] = text
	return p
}

// SourceText returns the registered source text for a file.
func (p *Program) SourceText(file string) (string, bool) {
	t, ok := p.sources[file]
	return t, ok
}

// Global sets a global variable.
func (p *Program) Global(name string, v *Value) *Program {
	p.globals[name] = v
	return p
}

// Func defines a new function and returns it for building.
func (p *Program) Func(name, file string) *FuncDef {
	f := &FuncDef{Name: name, File: file}
	p.funcs[name] = f
	p.order = append(p.order, name)
	return f
}

// FuncDefOf returns the function with the given name.
func (p *Program) FuncDefOf(name string) (*FuncDef, bool) {
	f, ok := p.funcs[name]
	return f, ok
}

// FuncNames returns function definition order.
func (p *Program) FuncNames() []string {
	out := make([]string, len(p.order))
	copy(out, p.order)
	return out
}

// ---- FuncDef builders ----

// Op appends an operation on the given line.
func (f *FuncDef) Op(line int, action OpAction) *FuncDef {
	f.Ops = append(f.Ops, Op{Line: line, Action: action})
	return f
}

// Nop appends a no-op (line advance) on the given line.
func (f *FuncDef) Nop(line int) *FuncDef {
	return f.Op(line, ActNop)
}

// Call appends a call operation.
func (f *FuncDef) Call(line int, callee string, args ...*Value) *FuncDef {
	f.Ops = append(f.Ops, Op{Line: line, Action: ActCall, Call: callee, Args: args})
	return f
}

// Return appends a return operation.
func (f *FuncDef) Return(line int) *FuncDef {
	return f.Op(line, ActReturn)
}

// Throw appends a throw operation carrying the given value.
func (f *FuncDef) Throw(line int, v *Value) *FuncDef {
	f.Ops = append(f.Ops, Op{Line: line, Action: ActThrow, Thrown: v})
	return f
}

// Fatal appends a fatal crash operation.
func (f *FuncDef) Fatal(line int, msg string) *FuncDef {
	f.Ops = append(f.Ops, Op{Line: line, Action: ActFatal, Thrown: StringValue(msg)})
	return f
}

// Loop appends a bounded jump: from this line to targetLine, at most count
// times, then execution falls through to the next line.
func (f *FuncDef) Loop(line, targetLine, count int) *FuncDef {
	f.Ops = append(f.Ops, Op{Line: line, Action: ActJumpN, JumpLine: targetLine, JumpCount: count})
	return f
}

// Param declares parameter names (bound positionally at call time).
func (f *FuncDef) Param(names ...string) *FuncDef {
	f.Params = append(f.Params, names...)
	return f
}

// Local declares local variable names (default to nil).
func (f *FuncDef) Local(names ...string) *FuncDef {
	f.Locals = append(f.Locals, names...)
	return f
}

// String renders a function signature.
func (f *FuncDef) String() string {
	return fmt.Sprintf("func %s(%v)", f.Name, f.Params)
}

// GlobalOf returns the global variable with the given name.
func (p *Program) GlobalOf(name string) (*Value, bool) {
	v, ok := p.globals[name]
	return v, ok
}

// Globals returns the raw globals map (read-only use by the debugger).
func (p *Program) Globals() map[string]*Value {
	return p.globals
}
