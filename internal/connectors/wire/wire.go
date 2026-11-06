package wire

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
)

const (
	Version = "aura.connector/v1"

	MaxSteps = 16

	typeDescribe   = "describe"
	typeInvoke     = "invoke"
	typeHTTPResult = "http_result"
	typeTools      = "tools"
	typeHTTP       = "http"
	typeResult     = "result"
	typeError      = "error"

	maxLineBytes    = 4 << 20
	initialLine     = 64 << 10
	fallbackCode    = "internal_error"
	maxFailureBytes = 48
)

var codePattern = regexp.MustCompile(`^[a-z_]{1,48}$`)

type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

type Call struct {
	Tool           string
	Arguments      json.RawMessage
	IdempotencyKey string
}

type Request struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    []byte
}

type Response struct {
	Status  int
	Headers map[string]string
	Body    []byte
	Failure string
}

type Do func(ctx context.Context, req *Request) (*Response, error)

type Handler interface {
	Tools() []Tool
	Invoke(ctx context.Context, call *Call, do Do) (json.RawMessage, error)
}

type Coder interface {
	ErrorCode() string
}

type Code string

const (
	CodeClosed    Code = "closed"
	CodeProtocol  Code = "protocol"
	CodeStepBound Code = "step_bound"
)

type Error struct {
	Code Code
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return "connector wire: " + string(e.Code)
}

func (e *Error) ErrorCode() string {
	if e == nil {
		return fallbackCode
	}
	return string(e.Code)
}

func CodeOf(err error) (Code, bool) {
	var target *Error
	if errors.As(err, &target) && target != nil {
		return target.Code, true
	}
	return "", false
}

func fail(code Code) error {
	return &Error{Code: code}
}

type hostMessage struct {
	V                string            `json:"v"`
	Type             string            `json:"type"`
	HostCapabilities []string          `json:"host_capabilities"`
	Call             string            `json:"call"`
	Tool             string            `json:"tool"`
	Arguments        json.RawMessage   `json:"arguments"`
	IdempotencyKey   string            `json:"idempotency_key"`
	Status           int               `json:"status"`
	Headers          map[string]string `json:"headers"`
	BodyB64          string            `json:"body_b64"`
	Error            string            `json:"error"`
}

type describedTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type toolsMessage struct {
	V            string          `json:"v"`
	Type         string          `json:"type"`
	Tools        []describedTool `json:"tools"`
	Capabilities []string        `json:"capabilities"`
}

type httpMessage struct {
	V       string            `json:"v"`
	Type    string            `json:"type"`
	Call    string            `json:"call"`
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	BodyB64 string            `json:"body_b64"`
}

type resultMessage struct {
	V      string          `json:"v"`
	Type   string          `json:"type"`
	Call   string          `json:"call"`
	Output json.RawMessage `json:"output"`
}

type errorMessage struct {
	V    string `json:"v"`
	Type string `json:"type"`
	Call string `json:"call"`
	Code string `json:"code"`
}

type stream struct {
	scanner *bufio.Scanner
	out     *bufio.Writer
}

func Serve(ctx context.Context, in io.Reader, out io.Writer, handler Handler) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, initialLine), maxLineBytes)
	s := &stream{scanner: scanner, out: bufio.NewWriter(out)}
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("connector wire: %w", err)
		}
		message, err := s.read()
		if err != nil {
			if codeIs(err, CodeClosed) {
				return nil
			}
			return err
		}
		switch message.Type {
		case typeDescribe:
			err = s.describe(handler)
		case typeInvoke:
			err = s.invoke(ctx, handler, message)
		default:
			err = fail(CodeProtocol)
		}
		if err != nil {
			if codeIs(err, CodeClosed) {
				return nil
			}
			return err
		}
	}
}

func codeIs(err error, want Code) bool {
	code, ok := CodeOf(err)
	return ok && code == want
}

func (s *stream) read() (*hostMessage, error) {
	if !s.scanner.Scan() {
		if s.scanner.Err() != nil {
			return nil, fail(CodeProtocol)
		}
		return nil, fail(CodeClosed)
	}
	decoder := json.NewDecoder(bytes.NewReader(s.scanner.Bytes()))
	decoder.DisallowUnknownFields()
	var message hostMessage
	if err := decoder.Decode(&message); err != nil || message.V != Version {
		return nil, fail(CodeProtocol)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, fail(CodeProtocol)
	}
	return &message, nil
}

func (s *stream) write(message any) error {
	encoder := json.NewEncoder(s.out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(message); err != nil {
		return fail(CodeClosed)
	}
	if err := s.out.Flush(); err != nil {
		return fail(CodeClosed)
	}
	return nil
}

func (s *stream) describe(handler Handler) error {
	tools := handler.Tools()
	described := make([]describedTool, 0, len(tools))
	for i := range tools {
		described = append(described, describedTool(tools[i]))
	}
	return s.write(&toolsMessage{V: Version, Type: typeTools, Tools: described, Capabilities: []string{}})
}

func (s *stream) invoke(ctx context.Context, handler Handler, message *hostMessage) error {
	if message.Call == "" {
		return fail(CodeProtocol)
	}
	steps := 0
	do := func(ctx context.Context, req *Request) (*Response, error) {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("connector wire: %w", err)
		}
		steps++
		if steps > MaxSteps {
			return nil, fail(CodeStepBound)
		}
		return s.exchange(message.Call, req)
	}
	output, err := handler.Invoke(ctx, &Call{Tool: message.Tool, Arguments: message.Arguments, IdempotencyKey: message.IdempotencyKey}, do)
	if err != nil {
		var wireErr *Error
		if errors.As(err, &wireErr) && wireErr.Code != CodeStepBound {
			return wireErr
		}
		return s.write(&errorMessage{V: Version, Type: typeError, Call: message.Call, Code: reportedCode(err)})
	}
	if len(bytes.TrimSpace(output)) == 0 || !json.Valid(output) {
		return s.write(&errorMessage{V: Version, Type: typeError, Call: message.Call, Code: fallbackCode})
	}
	return s.write(&resultMessage{V: Version, Type: typeResult, Call: message.Call, Output: output})
}

func reportedCode(err error) string {
	var coder Coder
	if errors.As(err, &coder) {
		if code := coder.ErrorCode(); codePattern.MatchString(code) {
			return code
		}
	}
	return fallbackCode
}

func (s *stream) exchange(call string, req *Request) (*Response, error) {
	headers := req.Headers
	if headers == nil {
		headers = map[string]string{}
	}
	if err := s.write(&httpMessage{
		V: Version, Type: typeHTTP, Call: call, Method: req.Method, URL: req.URL, Headers: headers,
		BodyB64: base64.StdEncoding.EncodeToString(req.Body),
	}); err != nil {
		return nil, err
	}
	reply, err := s.read()
	if err != nil {
		return nil, err
	}
	if reply.Type != typeHTTPResult || reply.Call != call {
		return nil, fail(CodeProtocol)
	}
	if len(reply.Error) > maxFailureBytes {
		return nil, fail(CodeProtocol)
	}
	body, err := base64.StdEncoding.Strict().DecodeString(reply.BodyB64)
	if err != nil {
		return nil, fail(CodeProtocol)
	}
	return &Response{Status: reply.Status, Headers: reply.Headers, Body: body, Failure: reply.Error}, nil
}
