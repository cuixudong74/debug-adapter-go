package debug

// stepMode describes an in-flight stepping operation for one task.
type stepMode int

const (
	stepNone stepMode = iota
	stepInto
	stepOver
)

// stepState tracks per-task stepping for a session.
type stepState struct {
	mode  stepMode
	depth int // call depth captured when the step started
}
