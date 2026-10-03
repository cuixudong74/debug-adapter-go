package debug

import (
	"errors"
	"fmt"
)

// ErrorKind identifies a class of debug-service error. Clients can switch on
// Kind to distinguish recoverable evaluation failures from protocol or state
// errors without parsing messages.
type ErrorKind string

const (
	KindNotFound        ErrorKind = "not_found"
	KindInvalidState    ErrorKind = "invalid_state"
	KindPermission      ErrorKind = "permission_denied"
	KindEval            ErrorKind = "eval_error"
	KindTimeout         ErrorKind = "timeout"
	KindCanceled        ErrorKind = "canceled"
	KindAlreadyAttached ErrorKind = "already_attached"
	KindNotAttached     ErrorKind = "not_attached"
	KindTerminated      ErrorKind = "terminated"
	KindInvalidArg      ErrorKind = "invalid_argument"
	KindQueueFull       ErrorKind = "queue_full"
)

// Error is the typed error returned by the debug service.
type Error struct {
	Kind ErrorKind
	Msg  string
	// EvalKind further describes evaluation failures (parse/notFound/type/index).
	EvalKind string
	Cause    error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Kind, e.Msg, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Msg)
}

func (e *Error) Unwrap() error { return e.Cause }

func newError(kind ErrorKind, msg string) *Error {
	return &Error{Kind: kind, Msg: msg}
}

func errorf(kind ErrorKind, format string, args ...any) *Error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, args...)}
}

// IsKind reports whether err is a debug Error of the given kind.
func IsKind(err error, kind ErrorKind) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind == kind
	}
	return false
}

// EvalKindOf returns the eval sub-kind of an eval error, or "".
func EvalKindOf(err error) string {
	var e *Error
	if errors.As(err, &e) && e.Kind == KindEval {
		return e.EvalKind
	}
	return ""
}
