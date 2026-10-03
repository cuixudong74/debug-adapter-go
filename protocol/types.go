// Package protocol defines the debug adapter wire protocol. It is a small
// JSON message protocol (request/response/event) with explicit version
// negotiation and backward-compatible field handling: fields added after the
// initial release are optional pointers, unknown fields are ignored, and old
// clients that omit new fields continue to work with server defaults.
package protocol

import "encoding/json"

// ProtocolVersion is the current protocol version.
const ProtocolVersion = "1.0"

// Message types.
const (
	TypeRequest  = "request"
	TypeResponse = "response"
	TypeEvent    = "event"
)

// Commands.
const (
	CmdInitialize             = "initialize"
	CmdLaunch                 = "launch"
	CmdAttach                 = "attach"
	CmdDetach                 = "detach"
	CmdTerminate              = "terminate"
	CmdSetBreakpoints         = "setBreakpoints"
	CmdSetExceptionBreakpoints = "setExceptionBreakpoints"
	CmdContinue               = "continue"
	CmdNext                   = "next"
	CmdStepIn                 = "stepIn"
	CmdStepOut                = "stepOut"
	CmdPause                  = "pause"
	CmdEvaluate               = "evaluate"
	CmdStackTrace             = "stackTrace"
	CmdScopes                 = "scopes"
	CmdVariables              = "variables"
	CmdThreads                = "threads"
)

// Message is the common envelope.
type Message struct {
	Seq  int    `json:"seq"`
	Type string `json:"type"`
}

// Request is a client->server command.
type Request struct {
	Message
	Command string          `json:"command"`
	Body    json.RawMessage `json:"body,omitempty"`
}

// Response is a server->client reply.
type Response struct {
	Message
	RequestSeq int             `json:"request_seq"`
	Success    bool            `json:"success"`
	Command    string          `json:"command"`
	Body       json.RawMessage `json:"body,omitempty"`
	ErrMessage string          `json:"message,omitempty"`
}

// Event is a server->client notification.
type Event struct {
	Message
	Event string          `json:"event"`
	Body  json.RawMessage `json:"body,omitempty"`
}

// ---- initialize ----

// InitializeRequest is the initialize command body. New fields are optional
// pointers so old clients that omit them keep working with server defaults.
type InitializeRequest struct {
	ClientID        string `json:"clientID,omitempty"`
	ClientName      string `json:"clientName,omitempty"`
	AdapterID       string `json:"adapterID,omitempty"`
	ProtocolVersion string `json:"protocolVersion,omitempty"`

	// Optional capability hints introduced after the initial release.
	SupportsSensitiveVariables *bool `json:"supportsSensitiveVariables,omitempty"`
	SupportsInProcessAttach    *bool `json:"supportsInProcessAttach,omitempty"`
}

// Capabilities is the initialize response body.
type Capabilities struct {
	ProtocolVersion                string `json:"protocolVersion"`
	SupportsConditionalBreakpoints *bool  `json:"supportsConditionalBreakpoints,omitempty"`
	SupportsExceptionBreakpoints   *bool  `json:"supportsExceptionBreakpoints,omitempty"`
	SupportsSensitiveVariables     *bool  `json:"supportsSensitiveVariables,omitempty"`
	SupportsInProcessAttach        *bool  `json:"supportsInProcessAttach,omitempty"`
	SupportsEvaluateForHovers      *bool  `json:"supportsEvaluateForHovers,omitempty"`
}

// ---- launch / attach ----

// LaunchRequest is the launch command body.
type LaunchRequest struct {
	Name     string `json:"name"`
	Program  string `json:"program"` // program source name
	UserID   string `json:"userID,omitempty"`
	UserName string `json:"userName,omitempty"`
}

// AttachRequest is the attach command body.
type AttachRequest struct {
	TargetID string `json:"targetID"`
	UserID   string `json:"userID,omitempty"`
	UserName string `json:"userName,omitempty"`
}

// AttachResponse is returned by launch/attach.
type AttachResponse struct {
	SessionID string `json:"sessionID"`
	TargetID  string `json:"targetID"`
}

// ---- breakpoints ----

// SourceBreakpoint is a requested breakpoint.
type SourceBreakpoint struct {
	Line      int    `json:"line"`
	Condition string `json:"condition,omitempty"`
}

// SetBreakpointsRequest is the setBreakpoints command body.
type SetBreakpointsRequest struct {
	File        string             `json:"file"`
	Breakpoints []SourceBreakpoint `json:"breakpoints"`
}

// Breakpoint is a reported breakpoint.
type Breakpoint struct {
	ID        int    `json:"id"`
	File      string `json:"file"`
	Line      int    `json:"line"`
	Condition string `json:"condition,omitempty"`
	Verified  bool   `json:"verified"`
	HitCount  int    `json:"hitCount,omitempty"`
}

// SetBreakpointsResponse is the setBreakpoints reply.
type SetBreakpointsResponse struct {
	Breakpoints []Breakpoint `json:"breakpoints"`
}

// SetExceptionBreakpointsRequest configures exception stops.
type SetExceptionBreakpointsRequest struct {
	Filters []string `json:"filters"` // "none" | "uncaught" | "all"
}

// ---- control ----

// ContinueRequest is the continue command body.
type ContinueRequest struct {
	TaskID int `json:"taskId,omitempty"`
}

// StepRequest is the next/stepIn/stepOut command body.
type StepRequest struct {
	TaskID int `json:"taskId"`
}

// PauseRequest is the pause command body.
type PauseRequest struct {
	TaskID int `json:"taskId"`
}

// ---- evaluation ----

// EvaluateRequest is the evaluate command body.
type EvaluateRequest struct {
	Expression string `json:"expression"`
	FrameID    int    `json:"frameId,omitempty"`
	Context    string `json:"context,omitempty"`
}

// EvaluateResponse is the evaluate reply.
type EvaluateResponse struct {
	Result             string `json:"result"`
	Type               string `json:"type"`
	VariablesReference int    `json:"variablesReference"`
	Sensitive          bool   `json:"sensitive,omitempty"`
}

// ---- inspection ----

// StackTraceRequest is the stackTrace command body.
type StackTraceRequest struct {
	TaskID int `json:"taskId"`
}

// StackFrame is a single frame.
type StackFrame struct {
	ID              int    `json:"id"`
	Name            string `json:"name"`
	File            string `json:"file"`
	Line            int    `json:"line"`
	LocalsReference int    `json:"localsReference"`
}

// StackTraceResponse is the stackTrace reply.
type StackTraceResponse struct {
	StackFrames []StackFrame `json:"stackFrames"`
}

// ScopesRequest is the scopes command body.
type ScopesRequest struct {
	FrameID int `json:"frameId"`
}

// Scope is a variable scope.
type Scope struct {
	Name               string `json:"name"`
	VariablesReference int    `json:"variablesReference"`
	Expensive          bool   `json:"expensive"`
}

// ScopesResponse is the scopes reply.
type ScopesResponse struct {
	Scopes []Scope `json:"scopes"`
}

// VariablesRequest is the variables command body.
type VariablesRequest struct {
	VariablesReference int `json:"variablesReference"`
}

// Variable is a single variable.
type Variable struct {
	Name               string `json:"name"`
	Value              string `json:"value"`
	Type               string `json:"type"`
	VariablesReference int    `json:"variablesReference"`
	Sensitive          bool   `json:"sensitive,omitempty"`
}

// VariablesResponse is the variables reply.
type VariablesResponse struct {
	Variables []Variable `json:"variables"`
}

// Thread is a task/thread.
type Thread struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// ThreadsResponse is the threads reply.
type ThreadsResponse struct {
	Threads []Thread `json:"threads"`
}

// ---- events ----

// StoppedEventBody is the stopped event body.
type StoppedEventBody struct {
	TaskID       int    `json:"taskId"`
	Reason       string `json:"reason"`
	BreakpointID int    `json:"breakpointId,omitempty"`
}

// ContinuedEventBody is the continued event body.
type ContinuedEventBody struct {
	TaskID int `json:"taskId,omitempty"`
}

// TerminatedEventBody is the terminated event body.
type TerminatedEventBody struct {
	Fatal     bool   `json:"fatal,omitempty"`
	Exception string `json:"exception,omitempty"`
}

// ThreadEventBody is the thread event body.
type ThreadEventBody struct {
	TaskID int    `json:"taskId"`
	Status string `json:"status"` // "started" | "exited"
}

// DiagnosticEventBody is the diagnostic event body.
type DiagnosticEventBody struct {
	Message string `json:"message"`
}
