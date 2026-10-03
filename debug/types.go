// Package debug implements an in-process debugging service for the vm
// runtime: sessions, breakpoints, stepping, stack/variable inspection
// and side-effect-free expression evaluation.
package debug

// ProtocolVersion is bumped when fields are added. All new fields are
// optional so older clients keep working (backward compatible).
const ProtocolVersion = 1

// StopReason explains why a task stopped.
type StopReason string

const (
	ReasonBreakpoint StopReason = "breakpoint"
	ReasonStep       StopReason = "step"
	ReasonException  StopReason = "exception"
	ReasonPause      StopReason = "pause"
)

// Breakpoint is a line breakpoint, optionally conditional.
type Breakpoint struct {
	ID        int    `json:"id"`
	Function  string `json:"function"`
	Line      int    `json:"line"`
	Condition string `json:"condition,omitempty"` // optional expression
}

// StackFrame is one frame of a paused task's call stack.
type StackFrame struct {
	ID       int    `json:"id"`
	TaskID   int    `json:"taskId"`
	Function string `json:"function"`
	Line     int    `json:"line"`
}

// Variable is a rendered variable; children are fetched lazily via
// Session.Variables to avoid side effects and unbounded expansion.
type Variable struct {
	Name        string `json:"name"`
	Value       string `json:"value"`
	Type        string `json:"type"`
	HasChildren bool   `json:"hasChildren"`
	Sensitive   bool   `json:"sensitive,omitempty"` // added in v1; absent means false
}

// EventType classifies debug events.
type EventType string

const (
	EventStopped    EventType = "stopped"
	EventContinued  EventType = "continued"
	EventTaskStart  EventType = "taskStarted"
	EventTaskEnd    EventType = "taskEnded"
	EventException  EventType = "exception"
	EventOverflow   EventType = "overflow"   // events were dropped
	EventTerminated EventType = "terminated" // session terminated
)

// Event is delivered on a session's bounded event channel.
type Event struct {
	Seq     int64      `json:"seq"`
	Type    EventType  `json:"type"`
	TaskID  int        `json:"taskId,omitempty"`
	Reason  StopReason `json:"reason,omitempty"`
	Message string     `json:"message,omitempty"`
	Dropped int        `json:"dropped,omitempty"` // overflow count
}

// AttachRequest configures a new session. Pointer fields are optional;
// old clients may omit them entirely.
type AttachRequest struct {
	Target         string `json:"target"`
	Role           string `json:"role,omitempty"`           // "admin" or "viewer" (default)
	ExceptionBreak *bool  `json:"exceptionBreak,omitempty"` // default true
}

// AuditEntry records a security-relevant action (e.g. denied read).
type AuditEntry struct {
	Seq       int64  `json:"seq"`
	SessionID int64  `json:"sessionId"`
	Action    string `json:"action"`
	Detail    string `json:"detail"`
	Allowed   bool   `json:"allowed"`
}

// ErrUndefinedSymbol marks evaluation failures caused by unknown names
// or keys; callers can distinguish them from protocol errors.
var ErrUndefinedSymbol = errUndefined{}

type errUndefined struct{}

func (errUndefined) Error() string { return "undefined symbol" }
