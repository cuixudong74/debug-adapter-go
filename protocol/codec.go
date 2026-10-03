package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Encode marshals a message to JSON.
func Encode(m any) ([]byte, error) {
	return json.Marshal(m)
}

// DecodeRequest decodes a request, tolerating unknown fields (old/new clients
// with extra fields keep working).
func DecodeRequest(data []byte) (*Request, error) {
	var req Request
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&req); err != nil {
		return nil, fmt.Errorf("decode request: %w", err)
	}
	return &req, nil
}

// DecodeEvent decodes an event body into the given type.
func DecodeEventBody(body json.RawMessage, v any) error {
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}

// DecodeBody decodes a request body into the given type.
func DecodeBody(body json.RawMessage, v any) error {
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}

// NegotiateVersion selects the protocol version for a client. Clients that
// omit protocolVersion are treated as 1.0 (the initial release). A client
// requesting a different major version is rejected.
func NegotiateVersion(clientVersion string) (string, error) {
	if clientVersion == "" {
		return ProtocolVersion, nil
	}
	clientMajor := strings.SplitN(clientVersion, ".", 2)[0]
	serverMajor := strings.SplitN(ProtocolVersion, ".", 2)[0]
	if clientMajor != serverMajor {
		return "", fmt.Errorf("unsupported protocol version %q (server %s)", clientVersion, ProtocolVersion)
	}
	return ProtocolVersion, nil
}

// NewResponse builds a success response for a request.
func NewResponse(req *Request, body any) (*Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &Response{
		Message:    Message{Seq: req.Seq, Type: TypeResponse},
		RequestSeq: req.Seq,
		Success:    true,
		Command:    req.Command,
		Body:       raw,
	}, nil
}

// NewErrorResponse builds an error response for a request.
func NewErrorResponse(req *Request, errMsg string) *Response {
	return &Response{
		Message:    Message{Seq: req.Seq, Type: TypeResponse},
		RequestSeq: req.Seq,
		Success:    false,
		Command:    req.Command,
		ErrMessage: errMsg,
	}
}

// NewEvent builds an event.
func NewEvent(seq int, event string, body any) (*Event, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &Event{
		Message: Message{Seq: seq, Type: TypeEvent},
		Event:   event,
		Body:    raw,
	}, nil
}
