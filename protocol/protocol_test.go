package protocol

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cuixudong74/debug-adapter-go/debug"
	"github.com/cuixudong74/debug-adapter-go/runtime"
)

func testProgram() *runtime.Program {
	p := runtime.NewProgram("test")
	p.Func("main", "m.go").Nop(1).Call(2, "helper").Nop(3).Return(4)
	p.Func("helper", "h.go").Nop(1).Return(2)
	return p
}

func newHandler() *Handler {
	svc := debug.NewService()
	svc.SetYield(func() { time.Sleep(2 * time.Millisecond) })
	h := NewHandler(svc)
	h.RegisterProgram("test", testProgram())
	return h
}

func dispatch(t *testing.T, h *Handler, req *Request) *Response {
	t.Helper()
	resp, err := h.Dispatch(context.Background(), req)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !resp.Success {
		t.Fatalf("dispatch failed: %s", resp.ErrMessage)
	}
	return resp
}

func decodeBody(t *testing.T, raw json.RawMessage, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decode body: %v", err)
	}
}

func TestInitializeOldClientDefaults(t *testing.T) {
	h := newHandler()
	// old client: no protocolVersion, no optional fields
	req := &Request{Message: Message{Seq: 1, Type: TypeRequest}, Command: CmdInitialize}
	resp := dispatch(t, h, req)
	var caps Capabilities
	decodeBody(t, resp.Body, &caps)
	if caps.ProtocolVersion != ProtocolVersion {
		t.Fatalf("expected version %s, got %s", ProtocolVersion, caps.ProtocolVersion)
	}
	// server defaults are all enabled
	if caps.SupportsConditionalBreakpoints == nil || !*caps.SupportsConditionalBreakpoints {
		t.Fatal("conditional breakpoints should be enabled by default")
	}
}

func TestInitializeNewClientFields(t *testing.T) {
	h := newHandler()
	sens := true
	body, _ := json.Marshal(InitializeRequest{
		ClientID: "editor", ClientName: "test", ProtocolVersion: "1.0",
		SupportsSensitiveVariables: &sens, SupportsInProcessAttach: &sens,
	})
	req := &Request{Message: Message{Seq: 1, Type: TypeRequest}, Command: CmdInitialize, Body: body}
	resp := dispatch(t, h, req)
	var caps Capabilities
	decodeBody(t, resp.Body, &caps)
	if caps.ProtocolVersion != ProtocolVersion {
		t.Fatalf("expected %s, got %s", ProtocolVersion, caps.ProtocolVersion)
	}
}

func TestVersionRejection(t *testing.T) {
	h := newHandler()
	body, _ := json.Marshal(InitializeRequest{ProtocolVersion: "2.0"})
	req := &Request{Message: Message{Seq: 1, Type: TypeRequest}, Command: CmdInitialize, Body: body}
	resp, err := h.Dispatch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Success {
		t.Fatal("major version mismatch should be rejected")
	}
}

func TestUnknownFieldsIgnored(t *testing.T) {
	// old client payload with extra unknown fields must decode fine
	raw := `{"seq":1,"type":"request","command":"initialize","body":{"clientID":"x","unknownNewField":42,"nested":{"a":1}}}`
	req, err := DecodeRequest([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if req.Command != CmdInitialize {
		t.Fatalf("got command %s", req.Command)
	}
	h := newHandler()
	resp := dispatch(t, h, req)
	if !resp.Success {
		t.Fatalf("unknown fields should be ignored: %s", resp.ErrMessage)
	}
}

func TestFullProtocolFlow(t *testing.T) {
	h := newHandler()
	// launch
	launchBody, _ := json.Marshal(LaunchRequest{Name: "t1", Program: "test", UserID: "u1"})
	req := &Request{Message: Message{Seq: 1, Type: TypeRequest}, Command: CmdLaunch, Body: launchBody}
	resp := dispatch(t, h, req)
	var lr AttachResponse
	decodeBody(t, resp.Body, &lr)
	if lr.SessionID == "" || lr.TargetID == "" {
		t.Fatalf("launch response missing ids: %+v", lr)
	}

	// set breakpoint at m.go:2
	bpBody, _ := json.Marshal(SetBreakpointsRequest{
		File: "m.go",
		Breakpoints: []SourceBreakpoint{{Line: 2}},
	})
	req = &Request{Message: Message{Seq: 2, Type: TypeRequest}, Command: CmdSetBreakpoints, Body: bpBody}
	resp = dispatch(t, h, req)
	var bpr SetBreakpointsResponse
	decodeBody(t, resp.Body, &bpr)
	if len(bpr.Breakpoints) != 1 || bpr.Breakpoints[0].Line != 2 {
		t.Fatalf("unexpected bps: %+v", bpr)
	}

	// wait for stopped event via the session
	sess, _ := h.session(lr.SessionID)
	ev, err := sess.Events().Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != debug.EvStopped {
		t.Fatalf("expected stopped, got %s", ev.Kind)
	}

	// stack trace
	stBody, _ := json.Marshal(StackTraceRequest{TaskID: ev.TaskID})
	req = &Request{Message: Message{Seq: 3, Type: TypeRequest}, Command: CmdStackTrace, Body: stBody}
	resp = dispatch(t, h, req)
	var st StackTraceResponse
	decodeBody(t, resp.Body, &st)
	if len(st.StackFrames) != 1 || st.StackFrames[0].Name != "main" {
		t.Fatalf("unexpected stack: %+v", st)
	}

	// evaluate
	evalBody, _ := json.Marshal(EvaluateRequest{Expression: "1+2", FrameID: st.StackFrames[0].ID})
	req = &Request{Message: Message{Seq: 4, Type: TypeRequest}, Command: CmdEvaluate, Body: evalBody}
	resp = dispatch(t, h, req)
	var er EvaluateResponse
	decodeBody(t, resp.Body, &er)
	if er.Result != "3" {
		t.Fatalf("expected 3, got %s", er.Result)
	}

	// continue
	req = &Request{Message: Message{Seq: 5, Type: TypeRequest}, Command: CmdContinue}
	resp = dispatch(t, h, req)
	if !resp.Success {
		t.Fatalf("continue failed: %s", resp.ErrMessage)
	}

	// next event should be terminated (skip continued)
	for {
		ev, err = sess.Events().Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == debug.EvTerminated {
			break
		}
	}
}

func TestDetachLeavesTargetRunning(t *testing.T) {
	h := newHandler()
	launchBody, _ := json.Marshal(LaunchRequest{Name: "t", Program: "test", UserID: "u1"})
	req := &Request{Message: Message{Seq: 1, Type: TypeRequest}, Command: CmdLaunch, Body: launchBody}
	resp := dispatch(t, h, req)
	var lr AttachResponse
	decodeBody(t, resp.Body, &lr)

	// detach
	req = &Request{Message: Message{Seq: 2, Type: TypeRequest}, Command: CmdDetach}
	resp = dispatch(t, h, req)
	if !resp.Success {
		t.Fatalf("detach failed: %s", resp.ErrMessage)
	}
	sess, _ := h.session(lr.SessionID)
	if sess.State() != "detached" {
		t.Fatalf("expected detached, got %s", sess.State())
	}
	// target still running
	tgt, _ := h.svc.Target(lr.TargetID)
	if tgt == nil {
		t.Fatal("target should still exist")
	}
}

func TestNegotiateVersion(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"", false},
		{"1.0", false},
		{"1.1", false},
		{"2.0", true},
		{"0.9", true},
	}
	for _, c := range cases {
		_, err := NegotiateVersion(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("NegotiateVersion(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
		}
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	ev, err := NewEvent(1, "stopped", StoppedEventBody{TaskID: 1, Reason: "breakpoint"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := Encode(ev)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"event":"stopped"`) {
		t.Fatalf("missing event field: %s", data)
	}
	var decoded Event
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Event != "stopped" {
		t.Fatalf("got %s", decoded.Event)
	}
}
