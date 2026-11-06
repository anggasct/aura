package connector

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/plugin/auth"
)

const testToken = "tok-secret-9f2c"

type blockReply struct{}

type scriptedSession struct {
	mu       sync.Mutex
	handler  func(in map[string]any) (any, error)
	received [][]byte
	closed   atomic.Bool
}

func (s *scriptedSession) Request(ctx context.Context, payload []byte) ([]byte, error) {
	s.mu.Lock()
	s.received = append(s.received, bytes.Clone(payload))
	handler := s.handler
	s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var in map[string]any
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	reply, err := handler(in)
	if err != nil {
		return nil, err
	}
	switch typed := reply.(type) {
	case []byte:
		return typed, nil
	case blockReply:
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return json.Marshal(reply)
}

func (s *scriptedSession) Close(context.Context) error {
	s.closed.Store(true)
	return nil
}

func (s *scriptedSession) payloads() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.received...)
}

type scriptedStarter struct {
	mu       sync.Mutex
	handler  func(index int) func(in map[string]any) (any, error)
	failures map[int]error
	sessions []*scriptedSession
	requests []SessionRequest
}

func newStarter(handler func(index int) func(in map[string]any) (any, error)) *scriptedStarter {
	return &scriptedStarter{handler: handler, failures: map[int]error{}}
}

func (s *scriptedStarter) StartSession(_ context.Context, req *SessionRequest) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := len(s.requests)
	s.requests = append(s.requests, *req)
	if err := s.failures[index]; err != nil {
		return nil, err
	}
	session := &scriptedSession{handler: s.handler(index)}
	s.sessions = append(s.sessions, session)
	return session, nil
}

func (s *scriptedStarter) started() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *scriptedStarter) session(index int) *scriptedSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[index]
}

func (s *scriptedStarter) openSessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	open := 0
	for _, session := range s.sessions {
		if !session.closed.Load() {
			open++
		}
	}
	return open
}

func (s *scriptedStarter) allPayloads() [][]byte {
	s.mu.Lock()
	sessions := append([]*scriptedSession(nil), s.sessions...)
	s.mu.Unlock()
	var all [][]byte
	for _, session := range sessions {
		all = append(all, session.payloads()...)
	}
	return all
}

type bearerAuth struct {
	token string
	err   error
	calls atomic.Int64
}

func (a *bearerAuth) Ready(context.Context) error { return a.err }

func (a *bearerAuth) Authorize(_ context.Context, req *http.Request) error {
	a.calls.Add(1)
	if a.err != nil {
		return a.err
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	return nil
}

type recordedRequest struct {
	Method  string
	Path    string
	Query   string
	Header  http.Header
	Body    []byte
	Version string
}

type fakeAPI struct {
	server  *httptest.Server
	mu      sync.Mutex
	seen    []recordedRequest
	handler func(w http.ResponseWriter, r *http.Request)
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	api := &fakeAPI{}
	api.handler = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
	api.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		api.mu.Lock()
		api.seen = append(api.seen, recordedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: body})
		handler := api.handler
		api.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(api.server.Close)
	return api
}

func (a *fakeAPI) host() string {
	return strings.TrimPrefix(a.server.URL, "https://")
}

func (a *fakeAPI) url(path string) string {
	return a.server.URL + path
}

func (a *fakeAPI) requests() []recordedRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]recordedRequest(nil), a.seen...)
}

func (a *fakeAPI) count() int {
	return len(a.requests())
}

func (a *fakeAPI) client() *http.Client {
	return a.server.Client()
}

func describeReply(names ...string) map[string]any {
	described := make([]map[string]any, 0, len(names))
	for _, name := range names {
		described = append(described, map[string]any{
			"name": name, "description": "connector text for " + name, "input_schema": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}},
		})
	}
	return map[string]any{"v": Version, "type": "tools", "tools": described}
}

func httpReply(call, method, rawURL string, headers map[string]string, body []byte) map[string]any {
	return map[string]any{
		"v": Version, "type": "http", "call": call, "method": method, "url": rawURL, "headers": headers,
		"body_b64": base64.StdEncoding.EncodeToString(body),
	}
}

func resultReply(call string, output any) map[string]any {
	return map[string]any{"v": Version, "type": "result", "call": call, "output": output}
}

func errorReply(call, code string) map[string]any {
	return map[string]any{"v": Version, "type": "error", "call": call, "code": code}
}

func callOf(in map[string]any) string {
	call, _ := in["call"].(string)
	return call
}

func kindOf(in map[string]any) string {
	kind, _ := in["type"].(string)
	return kind
}

type program func(in map[string]any) (any, error)

func oneRequest(method, rawURL string, headers map[string]string, body []byte) program {
	return func(in map[string]any) (any, error) {
		switch kindOf(in) {
		case "describe":
			return describeReply("search", "send"), nil
		case "invoke":
			return httpReply(callOf(in), method, rawURL, headers, body), nil
		default:
			output := map[string]any{}
			for _, key := range []string{"status", "error", "headers"} {
				if value, present := in[key]; present {
					output[key] = value
				}
			}
			return resultReply(callOf(in), output), nil
		}
	}
}

func newTestHost(t *testing.T, api *fakeAPI, starter Starter, strategy bearerAuthLike, tools ...Tool) *host {
	t.Helper()
	cfg := testConfig(api, strategy, tools...)
	client := http.DefaultClient
	if api != nil {
		client = api.client()
	}
	return newHost(&cfg, starter, client, slog.New(slog.DiscardHandler))
}

type bearerAuthLike = interface {
	Ready(context.Context) error
	Authorize(context.Context, *http.Request) error
}

func testConfig(api *fakeAPI, strategy bearerAuthLike, tools ...Tool) Config {
	if len(tools) == 0 {
		tools = []Tool{{Name: "search", Description: "Search mail."}, {Name: "send", Description: "Send mail.", Effect: true}}
	}
	egressHosts := []string{"api.mail.example"}
	authHosts := []string{"api.mail.example"}
	if api != nil {
		egressHosts = append(egressHosts, api.host())
		authHosts = append(authHosts, api.host())
	}
	return Config{
		ID: "mailer", Executable: "/bundle/bin/connector", WorkingDir: "/bundle",
		StartupTimeout: 5 * time.Second, RequestTimeout: 20 * time.Second, MaxMessageBytes: 1 << 20,
		Egress: egressHosts, AuthHosts: authHosts, Tools: tools, Auth: strategy,
	}
}

func readTool(h *host) *Tool {
	return &h.cfg.Tools[0]
}

func effectTool(h *host) *Tool {
	return &h.cfg.Tools[1]
}

func invokeTool(t *testing.T, h *host, tool *Tool) (json.RawMessage, *Error) {
	t.Helper()
	return h.invoke(t.Context(), &call{tool: tool, arguments: json.RawMessage(`{"q":"hello"}`), idempotencyKey: "key-1"})
}

func mustCode(t *testing.T, fail *Error, want ErrorCode) {
	t.Helper()
	if fail == nil || fail.Code != want {
		t.Fatalf("failure = %v, want %s", fail, want)
	}
}

func decodeResultOutput(t *testing.T, output json.RawMessage) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(output, &decoded); err != nil {
		t.Fatalf("output %s: %v", output, err)
	}
	return decoded
}

var errSessionGone = errors.New("child exited")

func authErr(code ErrorCode) error {
	return auth.Errorf(auth.ErrorCode(code), "provider text")
}
