package wire

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type scriptedHandler struct {
	tools  []Tool
	invoke func(ctx context.Context, call *Call, do Do) (json.RawMessage, error)
}

func (h *scriptedHandler) Tools() []Tool { return h.tools }

func (h *scriptedHandler) Invoke(ctx context.Context, call *Call, do Do) (json.RawMessage, error) {
	return h.invoke(ctx, call, do)
}

type codedError string

func (e codedError) Error() string { return string(e) }

func (e codedError) ErrorCode() string { return string(e) }

func script(lines ...string) *bytes.Buffer {
	return bytes.NewBufferString(strings.Join(lines, "\n") + "\n")
}

func replies(t *testing.T, out *bytes.Buffer) []map[string]any {
	t.Helper()
	var all []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("reply %q: %v", line, err)
		}
		all = append(all, decoded)
	}
	return all
}

func b64(text string) string {
	return base64.StdEncoding.EncodeToString([]byte(text))
}

const (
	describeLine = `{"v":"aura.connector/v1","type":"describe","host_capabilities":[]}`
	invokeLine   = `{"v":"aura.connector/v1","type":"invoke","call":"c1","tool":"echo","arguments":{"a":1},"idempotency_key":"k1"}`
)

func TestServeDescribesToolsWithEmptyCapabilities(t *testing.T) {
	handler := &scriptedHandler{tools: []Tool{{Name: "echo", Description: "Echo.", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	out := &bytes.Buffer{}
	if err := Serve(t.Context(), script(describeLine), out, handler); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	got := replies(t, out)
	if len(got) != 1 || got[0]["type"] != "tools" || got[0]["v"] != Version {
		t.Fatalf("replies = %v", got)
	}
	capabilities, ok := got[0]["capabilities"].([]any)
	if !ok || len(capabilities) != 0 {
		t.Fatalf("capabilities = %v, want an empty list", got[0]["capabilities"])
	}
	tools, _ := got[0]["tools"].([]any)
	first, _ := tools[0].(map[string]any)
	if len(tools) != 1 || first["name"] != "echo" {
		t.Fatalf("tools = %v", got[0]["tools"])
	}
}

func TestServeRunsAnInvocationThroughHTTPExchanges(t *testing.T) {
	handler := &scriptedHandler{invoke: func(ctx context.Context, call *Call, do Do) (json.RawMessage, error) {
		if call.Tool != "echo" || call.IdempotencyKey != "k1" || string(call.Arguments) != `{"a":1}` {
			t.Errorf("call = %+v", call)
		}
		response, err := do(ctx, &Request{Method: "GET", URL: "https://example.test/x", Body: []byte("hi")})
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"status": response.Status, "body": string(response.Body), "failure": response.Failure})
	}}
	input := script(invokeLine, `{"v":"aura.connector/v1","type":"http_result","call":"c1","status":200,"body_b64":"`+b64("payload")+`"}`)
	out := &bytes.Buffer{}
	if err := Serve(t.Context(), input, out, handler); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	got := replies(t, out)
	if len(got) != 2 || got[0]["type"] != "http" || got[0]["call"] != "c1" || got[0]["method"] != "GET" || got[0]["body_b64"] != b64("hi") {
		t.Fatalf("replies = %v", got)
	}
	if got[1]["type"] != "result" || got[1]["call"] != "c1" {
		t.Fatalf("final reply = %v", got[1])
	}
	output, _ := got[1]["output"].(map[string]any)
	if output["status"] != float64(200) || output["body"] != "payload" {
		t.Fatalf("output = %v", output)
	}
}

func TestServeReportsHandlerFailuresAsCodesOnly(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"coded error", codedError("not_found"), "not_found"},
		{"code with a bad shape", codedError("Not Found!"), "internal_error"},
		{"plain error", errors.New("secret provider text"), "internal_error"},
		{"wrapped coded error", errors.Join(errors.New("context"), codedError("rate_limited")), "rate_limited"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := &scriptedHandler{invoke: func(context.Context, *Call, Do) (json.RawMessage, error) { return nil, tc.err }}
			out := &bytes.Buffer{}
			if err := Serve(t.Context(), script(invokeLine), out, handler); err != nil {
				t.Fatalf("Serve: %v", err)
			}
			got := replies(t, out)
			if len(got) != 1 || got[0]["type"] != "error" || got[0]["code"] != tc.want || strings.Contains(out.String(), "secret provider text") {
				t.Fatalf("replies = %v, want code %s", got, tc.want)
			}
		})
	}
}

func TestServeRefusesOutputThatIsNotJSON(t *testing.T) {
	handler := &scriptedHandler{invoke: func(context.Context, *Call, Do) (json.RawMessage, error) { return json.RawMessage("not json"), nil }}
	out := &bytes.Buffer{}
	if err := Serve(t.Context(), script(invokeLine), out, handler); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if got := replies(t, out); len(got) != 1 || got[0]["code"] != "internal_error" {
		t.Fatalf("replies = %v", got)
	}
}

func TestServeStopsAnInvocationThatExceedsTheStepBound(t *testing.T) {
	steps := 0
	handler := &scriptedHandler{invoke: func(ctx context.Context, _ *Call, do Do) (json.RawMessage, error) {
		for {
			if _, err := do(ctx, &Request{Method: "GET", URL: "https://example.test/x"}); err != nil {
				return nil, err
			}
			steps++
		}
	}}
	lines := []string{invokeLine}
	for range MaxSteps {
		lines = append(lines, `{"v":"aura.connector/v1","type":"http_result","call":"c1","status":200}`)
	}
	out := &bytes.Buffer{}
	if err := Serve(t.Context(), script(lines...), out, handler); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	got := replies(t, out)
	last := got[len(got)-1]
	if steps != MaxSteps || last["type"] != "error" || last["code"] != "step_bound" {
		t.Fatalf("steps = %d, last = %v", steps, last)
	}
}

func TestServeEndsQuietlyWhenTheHostClosesMidExchange(t *testing.T) {
	handler := &scriptedHandler{invoke: func(ctx context.Context, _ *Call, do Do) (json.RawMessage, error) {
		_, err := do(ctx, &Request{Method: "GET", URL: "https://example.test/x"})
		return nil, err
	}}
	out := &bytes.Buffer{}
	if err := Serve(t.Context(), script(invokeLine), out, handler); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if got := replies(t, out); len(got) != 1 || got[0]["type"] != "http" {
		t.Fatalf("replies = %v", got)
	}
}

func TestServeRefusesMessagesThatBreakTheContract(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"wrong version", `{"v":"aura.connector/v2","type":"describe","host_capabilities":[]}`},
		{"unknown field", `{"v":"aura.connector/v1","type":"describe","host_capabilities":[],"extra":1}`},
		{"unknown type", `{"v":"aura.connector/v1","type":"shutdown"}`},
		{"not json", `describe please`},
		{"trailing data", `{"v":"aura.connector/v1","type":"describe","host_capabilities":[]} {}`},
		{"invoke without a call", `{"v":"aura.connector/v1","type":"invoke","tool":"echo","arguments":{},"idempotency_key":"k"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := &scriptedHandler{}
			err := Serve(t.Context(), script(tc.line), &bytes.Buffer{}, handler)
			if code, ok := CodeOf(err); !ok || code != CodeProtocol {
				t.Fatalf("Serve = %v, want the protocol code", err)
			}
		})
	}
}

func TestServeRefusesAnHTTPResultForAnotherCall(t *testing.T) {
	handler := &scriptedHandler{invoke: func(ctx context.Context, _ *Call, do Do) (json.RawMessage, error) {
		_, err := do(ctx, &Request{Method: "GET", URL: "https://example.test/x"})
		return nil, err
	}}
	err := Serve(t.Context(), script(invokeLine, `{"v":"aura.connector/v1","type":"http_result","call":"other","status":200}`), &bytes.Buffer{}, handler)
	if code, ok := CodeOf(err); !ok || code != CodeProtocol {
		t.Fatalf("Serve = %v, want the protocol code", err)
	}
}

func TestServeCarriesAnHTTPFailureToTheHandler(t *testing.T) {
	var failure string
	handler := &scriptedHandler{invoke: func(ctx context.Context, _ *Call, do Do) (json.RawMessage, error) {
		response, err := do(ctx, &Request{Method: "GET", URL: "https://example.test/x"})
		if err != nil {
			return nil, err
		}
		failure = response.Failure
		return json.RawMessage(`{}`), nil
	}}
	input := script(invokeLine, `{"v":"aura.connector/v1","type":"http_result","call":"c1","error":"response_too_large"}`)
	if err := Serve(t.Context(), input, &bytes.Buffer{}, handler); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if failure != "response_too_large" {
		t.Fatalf("failure = %q", failure)
	}
}

func TestServeStopsWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := Serve(ctx, script(describeLine), &bytes.Buffer{}, &scriptedHandler{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve = %v, want context.Canceled", err)
	}
}

func TestServeKeepsMarkupCharactersInOutputReadable(t *testing.T) {
	handler := &scriptedHandler{invoke: func(context.Context, *Call, Do) (json.RawMessage, error) {
		return json.RawMessage(`{"text":"a <b> & c"}`), nil
	}}
	out := &bytes.Buffer{}
	if err := Serve(t.Context(), script(invokeLine), out, handler); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if !strings.Contains(out.String(), `a <b> & c`) {
		t.Fatalf("output = %s", out.String())
	}
}
