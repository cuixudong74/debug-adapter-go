package protocol

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/cuixudong74/debug-adapter-go/debug"
	"github.com/cuixudong74/debug-adapter-go/runtime"
)

// Handler dispatches protocol requests to the in-process debug service.
type Handler struct {
	svc      *debug.Service
	progMu   sync.Mutex
	programs map[string]*runtime.Program

	sessMu  sync.Mutex
	sessions map[string]*debug.Session
	defSess *debug.Session
}

// NewHandler creates a protocol handler.
func NewHandler(svc *debug.Service) *Handler {
	return &Handler{
		svc:      svc,
		programs: map[string]*runtime.Program{},
		sessions: map[string]*debug.Session{},
	}
}

// RegisterProgram registers a script program under a name.
func (h *Handler) RegisterProgram(name string, prog *runtime.Program) {
	h.progMu.Lock()
	h.programs[name] = prog
	h.progMu.Unlock()
}

func (h *Handler) program(name string) (*runtime.Program, bool) {
	h.progMu.Lock()
	defer h.progMu.Unlock()
	p, ok := h.programs[name]
	return p, ok
}

func (h *Handler) setSession(sess *debug.Session) {
	h.sessMu.Lock()
	h.sessions[sess.ID()] = sess
	h.defSess = sess
	h.sessMu.Unlock()
}

func (h *Handler) session(id string) (*debug.Session, bool) {
	h.sessMu.Lock()
	defer h.sessMu.Unlock()
	if id != "" {
		s, ok := h.sessions[id]
		return s, ok
	}
	return h.defSess, h.defSess != nil
}

// Dispatch handles a single request and returns a response.
func (h *Handler) Dispatch(ctx context.Context, req *Request) (*Response, error) {
	switch req.Command {
	case CmdInitialize:
		return h.initialize(ctx, req)
	case CmdLaunch:
		return h.launch(ctx, req)
	case CmdAttach:
		return h.attach(ctx, req)
	case CmdDetach:
		return h.detach(ctx, req)
	case CmdTerminate:
		return h.terminate(ctx, req)
	case CmdSetBreakpoints:
		return h.setBreakpoints(ctx, req)
	case CmdSetExceptionBreakpoints:
		return h.setExceptionBreakpoints(ctx, req)
	case CmdContinue:
		return h.continueCmd(ctx, req)
	case CmdNext:
		return h.next(ctx, req)
	case CmdStepIn:
		return h.stepIn(ctx, req)
	case CmdStepOut:
		return h.stepOut(ctx, req)
	case CmdPause:
		return h.pause(ctx, req)
	case CmdEvaluate:
		return h.evaluate(ctx, req)
	case CmdStackTrace:
		return h.stackTrace(ctx, req)
	case CmdScopes:
		return h.scopes(ctx, req)
	case CmdVariables:
		return h.variables(ctx, req)
	case CmdThreads:
		return h.threads(ctx, req)
	default:
		return NewErrorResponse(req, "unknown command: "+req.Command), nil
	}
}

func (h *Handler) initialize(ctx context.Context, req *Request) (*Response, error) {
	var body InitializeRequest
	if err := DecodeBody(req.Body, &body); err != nil {
		return NewErrorResponse(req, err.Error()), nil
	}
	version, err := NegotiateVersion(body.ProtocolVersion)
	if err != nil {
		return NewErrorResponse(req, err.Error()), nil
	}
	t := true
	caps := Capabilities{
		ProtocolVersion:                version,
		SupportsConditionalBreakpoints: &t,
		SupportsExceptionBreakpoints:   &t,
		SupportsSensitiveVariables:      &t,
		SupportsInProcessAttach:         &t,
		SupportsEvaluateForHovers:      &t,
	}
	return NewResponse(req, caps)
}

func (h *Handler) user(userID, userName string) debug.User {
	if userID == "" {
		userID = "default"
	}
	if userName == "" {
		userName = userID
	}
	return debug.User{ID: userID, Name: userName, Roles: []string{
		string(debug.ScopeControl),
		string(debug.ScopeReadVariables),
		string(debug.ScopeEvaluate),
		string(debug.ScopeReadSensitive),
	}}
}

func (h *Handler) launch(ctx context.Context, req *Request) (*Response, error) {
	var body LaunchRequest
	if err := DecodeBody(req.Body, &body); err != nil {
		return NewErrorResponse(req, err.Error()), nil
	}
	prog, ok := h.program(body.Program)
	if !ok {
		return NewErrorResponse(req, "unknown program: "+body.Program), nil
	}
	sess, err := h.svc.Launch(ctx, body.Name, prog, h.user(body.UserID, body.UserName))
	if err != nil {
		return NewErrorResponse(req, err.Error()), nil
	}
	h.setSession(sess)
	return NewResponse(req, AttachResponse{SessionID: sess.ID(), TargetID: sess.TargetID()})
}

func (h *Handler) attach(ctx context.Context, req *Request) (*Response, error) {
	var body AttachRequest
	if err := DecodeBody(req.Body, &body); err != nil {
		return NewErrorResponse(req, err.Error()), nil
	}
	sess, err := h.svc.Attach(ctx, body.TargetID, h.user(body.UserID, body.UserName))
	if err != nil {
		return NewErrorResponse(req, err.Error()), nil
	}
	h.setSession(sess)
	return NewResponse(req, AttachResponse{SessionID: sess.ID(), TargetID: sess.TargetID()})
}

func (h *Handler) sessionFromBody(req *Request) (*debug.Session, *Response) {
	var base struct {
		SessionID string `json:"sessionID"`
	}
	_ = DecodeBody(req.Body, &base)
	sess, ok := h.session(base.SessionID)
	if !ok {
		return nil, NewErrorResponse(req, "no active session")
	}
	return sess, nil
}

func (h *Handler) detach(ctx context.Context, req *Request) (*Response, error) {
	sess, resp := h.sessionFromBody(req)
	if resp != nil {
		return resp, nil
	}
	if err := sess.Detach(ctx); err != nil {
		return NewErrorResponse(req, err.Error()), nil
	}
	return NewResponse(req, struct{}{})
}

func (h *Handler) terminate(ctx context.Context, req *Request) (*Response, error) {
	sess, resp := h.sessionFromBody(req)
	if resp != nil {
		return resp, nil
	}
	if err := sess.Terminate(ctx); err != nil {
		return NewErrorResponse(req, err.Error()), nil
	}
	return NewResponse(req, struct{}{})
}

func (h *Handler) setBreakpoints(ctx context.Context, req *Request) (*Response, error) {
	var body SetBreakpointsRequest
	if err := DecodeBody(req.Body, &body); err != nil {
		return NewErrorResponse(req, err.Error()), nil
	}
	sess, resp := h.sessionFromBody(req)
	if resp != nil {
		return resp, nil
	}
	specs := make([]debug.BreakpointSpec, len(body.Breakpoints))
	for i, bp := range body.Breakpoints {
		specs[i] = debug.BreakpointSpec{Line: bp.Line, Condition: bp.Condition}
	}
	bps, err := sess.SetBreakpoints(ctx, body.File, specs)
	if err != nil {
		return NewErrorResponse(req, err.Error()), nil
	}
	out := make([]Breakpoint, len(bps))
	for i, bp := range bps {
		out[i] = Breakpoint{ID: bp.ID, File: bp.File, Line: bp.Line, Condition: bp.Condition, Verified: bp.Verified, HitCount: bp.HitCount}
	}
	return NewResponse(req, SetBreakpointsResponse{Breakpoints: out})
}

func (h *Handler) setExceptionBreakpoints(ctx context.Context, req *Request) (*Response, error) {
	var body SetExceptionBreakpointsRequest
	if err := DecodeBody(req.Body, &body); err != nil {
		return NewErrorResponse(req, err.Error()), nil
	}
	sess, resp := h.sessionFromBody(req)
	if resp != nil {
		return resp, nil
	}
	filter := debug.ExNone
	if len(body.Filters) > 0 {
		switch body.Filters[0] {
		case "uncaught":
			filter = debug.ExUncaught
		case "all":
			filter = debug.ExAll
		}
	}
	if err := sess.SetExceptionFilter(ctx, filter); err != nil {
		return NewErrorResponse(req, err.Error()), nil
	}
	return NewResponse(req, struct{}{})
}

func (h *Handler) continueCmd(ctx context.Context, req *Request) (*Response, error) {
	sess, resp := h.sessionFromBody(req)
	if resp != nil {
		return resp, nil
	}
	if err := sess.Continue(ctx); err != nil {
		return NewErrorResponse(req, err.Error()), nil
	}
	return NewResponse(req, struct{}{})
}

func (h *Handler) stepCmd(ctx context.Context, req *Request, fn func(context.Context, int) error) (*Response, error) {
	var body StepRequest
	if err := DecodeBody(req.Body, &body); err != nil {
		return NewErrorResponse(req, err.Error()), nil
	}
	_, resp := h.sessionFromBody(req)
	if resp != nil {
		return resp, nil
	}
	if err := fn(ctx, body.TaskID); err != nil {
		return NewErrorResponse(req, err.Error()), nil
	}
	return NewResponse(req, struct{}{})
}

func (h *Handler) next(ctx context.Context, req *Request) (*Response, error) {
	return h.stepCmd(ctx, req, func(ctx context.Context, id int) error {
		sess, _ := h.sessionFromBody(req)
		return sess.StepOver(ctx, id)
	})
}

func (h *Handler) stepIn(ctx context.Context, req *Request) (*Response, error) {
	return h.stepCmd(ctx, req, func(ctx context.Context, id int) error {
		sess, _ := h.sessionFromBody(req)
		return sess.StepIn(ctx, id)
	})
}

func (h *Handler) stepOut(ctx context.Context, req *Request) (*Response, error) {
	return h.stepCmd(ctx, req, func(ctx context.Context, id int) error {
		sess, _ := h.sessionFromBody(req)
		return sess.StepOut(ctx, id)
	})
}

func (h *Handler) pause(ctx context.Context, req *Request) (*Response, error) {
	var body PauseRequest
	if err := DecodeBody(req.Body, &body); err != nil {
		return NewErrorResponse(req, err.Error()), nil
	}
	sess, resp := h.sessionFromBody(req)
	if resp != nil {
		return resp, nil
	}
	if err := sess.Pause(ctx, body.TaskID); err != nil {
		return NewErrorResponse(req, err.Error()), nil
	}
	return NewResponse(req, struct{}{})
}

func (h *Handler) evaluate(ctx context.Context, req *Request) (*Response, error) {
	var body EvaluateRequest
	if err := DecodeBody(req.Body, &body); err != nil {
		return NewErrorResponse(req, err.Error()), nil
	}
	sess, resp := h.sessionFromBody(req)
	if resp != nil {
		return resp, nil
	}
	v, err := sess.Evaluate(ctx, body.Expression, body.FrameID)
	if err != nil {
		return NewErrorResponse(req, err.Error()), nil
	}
	return NewResponse(req, EvaluateResponse{
		Result: v.Value, Type: v.Type, VariablesReference: v.VariablesReference, Sensitive: v.Sensitive,
	})
}

func (h *Handler) stackTrace(ctx context.Context, req *Request) (*Response, error) {
	var body StackTraceRequest
	if err := DecodeBody(req.Body, &body); err != nil {
		return NewErrorResponse(req, err.Error()), nil
	}
	sess, resp := h.sessionFromBody(req)
	if resp != nil {
		return resp, nil
	}
	frames, err := sess.Stack(ctx, body.TaskID)
	if err != nil {
		return NewErrorResponse(req, err.Error()), nil
	}
	out := make([]StackFrame, len(frames))
	for i, f := range frames {
		out[i] = StackFrame{ID: f.ID, Name: f.Name, File: f.File, Line: f.Line, LocalsReference: f.LocalsReference}
	}
	return NewResponse(req, StackTraceResponse{StackFrames: out})
}

func (h *Handler) scopes(ctx context.Context, req *Request) (*Response, error) {
	var body ScopesRequest
	if err := DecodeBody(req.Body, &body); err != nil {
		return NewErrorResponse(req, err.Error()), nil
	}
	sess, resp := h.sessionFromBody(req)
	if resp != nil {
		return resp, nil
	}
	// scopes are derived from a frame's locals reference
	frames := sess.FrameRefs()
	ref, ok := frames[body.FrameID]
	if !ok {
		return NewErrorResponse(req, "unknown frame"), nil
	}
	return NewResponse(req, ScopesResponse{Scopes: []Scope{{Name: "locals", VariablesReference: ref, Expensive: false}}})
}

func (h *Handler) variables(ctx context.Context, req *Request) (*Response, error) {
	var body VariablesRequest
	if err := DecodeBody(req.Body, &body); err != nil {
		return NewErrorResponse(req, err.Error()), nil
	}
	sess, resp := h.sessionFromBody(req)
	if resp != nil {
		return resp, nil
	}
	vars, err := sess.Variables(ctx, body.VariablesReference)
	if err != nil {
		return NewErrorResponse(req, err.Error()), nil
	}
	out := make([]Variable, len(vars))
	for i, v := range vars {
		out[i] = Variable{Name: v.Name, Value: v.Value, Type: v.Type, VariablesReference: v.VariablesReference, Sensitive: v.Sensitive}
	}
	return NewResponse(req, VariablesResponse{Variables: out})
}

func (h *Handler) threads(ctx context.Context, req *Request) (*Response, error) {
	sess, resp := h.sessionFromBody(req)
	if resp != nil {
		return resp, nil
	}
	tasks := sess.Tasks()
	out := make([]Thread, len(tasks))
	for i, t := range tasks {
		out[i] = Thread{ID: t.ID, Name: t.Name}
	}
	return NewResponse(req, ThreadsResponse{Threads: out})
}

// Ensure json import is used (for RawMessage in types).
var _ = json.Marshal
