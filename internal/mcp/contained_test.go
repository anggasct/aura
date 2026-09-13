package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
)

type scriptedSession struct {
	mu        sync.Mutex
	pending   [][]byte
	requests  int
	closed    int
	onCall    func()
	toolNames []string
}

func (s *scriptedSession) toolListJSON() string {
	names := s.toolNames
	if len(names) == 0 {
		names = []string{"echo"}
	}
	entries := make([]string, 0, len(names))
	for _, name := range names {
		entries = append(entries, `{"name":"`+name+`","description":"`+name+` tool","inputSchema":{"type":"object"}}`)
	}
	return "[" + strings.Join(entries, ",") + "]"
}

func (s *scriptedSession) Request(_ context.Context, payload []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	for _, line := range strings.Split(string(payload), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var header rpcHeader
		if err := json.Unmarshal([]byte(line), &header); err != nil {
			return nil, errors.New("malformed frame")
		}
		id := rpcIDString(header.ID)
		switch header.Method {
		case "ping":
			s.pending = append(s.pending, []byte(`{"jsonrpc":"2.0","id":`+id+`,"result":{}}`))
		case "initialize":
			s.pending = append(s.pending, []byte(`{"jsonrpc":"2.0","id":`+id+`,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"scripted","version":"1"}}}`))
		case "tools/list":
			s.pending = append(s.pending, []byte(`{"jsonrpc":"2.0","id":`+id+`,"result":{"tools":`+s.toolListJSON()+`}}`))
		case "tools/call":
			if s.onCall != nil {
				s.onCall()
			}
			s.pending = append(s.pending,
				[]byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`),
				[]byte(`{"jsonrpc":"2.0","id":`+id+`,"result":{"content":[{"type":"text","text":"done"}]}}`),
			)
		case "notifications/initialized", "notifications/cancelled":
		default:
			s.pending = append(s.pending, []byte(`{"jsonrpc":"2.0","id":`+id+`,"error":{"code":-32601,"message":"unknown"}}`))
		}
	}
	if len(s.pending) == 0 {
		return nil, errors.New("no output pending")
	}
	out := s.pending[0]
	s.pending = s.pending[1:]
	return out, nil
}

func rpcIDString(id any) string {
	switch value := id.(type) {
	case string:
		raw, err := json.Marshal(value)
		if err != nil {
			return `""`
		}
		return string(raw)
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	default:
		return "null"
	}
}

func (s *scriptedSession) Close(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed++
	return nil
}

type failingSession struct {
	closed int
	err    error
}

func (s *failingSession) Request(_ context.Context, _ []byte) ([]byte, error) {
	return nil, s.err
}

func (s *failingSession) Close(_ context.Context) error {
	s.closed++
	return nil
}

type stubSessionStarter struct {
	mu       sync.Mutex
	requests []*ContainedSessionRequest
	session  ContainedSession
	err      error
}

func (s *stubSessionStarter) StartSession(_ context.Context, req *ContainedSessionRequest) (ContainedSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
	if s.err != nil {
		return nil, s.err
	}
	if s.session == nil {
		s.session = &scriptedSession{}
	}
	return s.session, nil
}

func containedStdioConfig() *config.MCPServer {
	return &config.MCPServer{
		Name:           "contained",
		Transport:      config.MCPTransportStdio,
		Command:        "/bin/contained-server",
		Args:           []string{"--stdio"},
		Environment:    map[string]string{"SERVER_MODE": "test"},
		RequestTimeout: config.Duration(10 * time.Second),
		StartupTimeout: config.Duration(10 * time.Second),
		MaxMessageSize: 1 << 20,
	}
}

func TestContainedStdioExchange(t *testing.T) {
	ctx := t.Context()
	starter := &stubSessionStarter{}
	client, err := NewClient(containedStdioConfig(), nil, WithSessionStarter(starter))
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	defer func() { _ = client.Close(t.Context()) }()
	if err := client.Connect(ctx, nil); err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	if len(starter.requests) != 1 {
		t.Fatalf("starter calls = %d, want 1", len(starter.requests))
	}
	req := starter.requests[0]
	if req.Executable != "/bin/contained-server" || len(req.Arguments) != 1 || req.Arguments[0] != "--stdio" {
		t.Errorf("request = %+v", req)
	}
	if len(req.Environment) != 1 || req.Environment["SERVER_MODE"] != "test" {
		t.Errorf("environment = %v, want exactly the declared map", req.Environment)
	}
	if req.Timeout <= 0 || req.MaxOutputBytes <= 0 {
		t.Errorf("limits = %+v", req)
	}
	tools, err := client.DiscoverTools(ctx)
	if err != nil {
		t.Fatalf("DiscoverTools(): %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v", tools)
	}
	args, err := json.Marshal(EchoInput{Message: "hi"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res, err := client.CallTool(ctx, "echo", args)
	if err != nil {
		t.Fatalf("CallTool(): %v", err)
	}
	if res.IsError {
		t.Fatal("tool call returned error result")
	}
}

func TestContainedSpontaneousLinesDelivered(t *testing.T) {
	ctx := t.Context()
	scripted := &scriptedSession{}
	calls := 0
	scripted.onCall = func() { calls++ }
	starter := &stubSessionStarter{session: scripted}
	client, err := NewClient(containedStdioConfig(), nil, WithSessionStarter(starter))
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	defer func() { _ = client.Close(t.Context()) }()
	if err := client.Connect(ctx, nil); err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	for range 3 {
		args, err := json.Marshal(EchoInput{Message: "hi"})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := client.CallTool(ctx, "echo", args); err != nil {
			t.Fatalf("CallTool(): %v", err)
		}
	}
	if calls != 3 {
		t.Errorf("server calls = %d, want 3 despite interleaved notifications", calls)
	}
}

func TestContainedUnavailableFailsClosed(t *testing.T) {
	ctx := t.Context()
	starter := &stubSessionStarter{err: errors.New("sandbox unavailable")}
	client, err := NewClient(containedStdioConfig(), nil, WithSessionStarter(starter))
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	defer func() { _ = client.Close(t.Context()) }()
	if err := client.Connect(ctx, nil); err == nil {
		t.Fatal("unavailable containment accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrServerUnavailable {
		t.Fatalf("code = %v, %v", code, ok)
	}
	if len(starter.requests) != 1 {
		t.Errorf("starter calls = %d", len(starter.requests))
	}
}

func TestContainedWithoutStarterFailsClosed(t *testing.T) {
	ctx := t.Context()
	client, err := NewClient(containedStdioConfig(), nil)
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	defer func() { _ = client.Close(t.Context()) }()
	if err := client.Connect(ctx, nil); err == nil {
		t.Fatal("starterless stdio accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrServerUnavailable {
		t.Fatalf("code = %v, %v", code, ok)
	}
}

func TestContainedCloseEndsSession(t *testing.T) {
	ctx := t.Context()
	starter := &stubSessionStarter{}
	client, err := NewClient(containedStdioConfig(), nil, WithSessionStarter(starter))
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	if err := client.Connect(ctx, nil); err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	if err := client.Close(ctx); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	starter.mu.Lock()
	defer starter.mu.Unlock()
	scripted, ok := starter.session.(*scriptedSession)
	if !ok || scripted == nil || scripted.closed != 1 {
		t.Error("contained session not closed exactly once")
	}
}

func TestContainedReconnectClosesPriorSession(t *testing.T) {
	ctx := t.Context()
	first := &scriptedSession{}
	second := &scriptedSession{}
	starter := &stubSessionStarter{session: first}
	cfg := containedStdioConfig()
	cfg.Restart = &config.MCPRestart{MaxAttempts: 3, Window: config.Duration(time.Minute)}
	client, err := NewClient(cfg, nil, WithSessionStarter(starter))
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	defer func() { _ = client.Close(t.Context()) }()
	if err := client.Connect(ctx, nil); err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	client.mu.Lock()
	if client.containedSession != ContainedSession(first) {
		client.mu.Unlock()
		t.Fatal("first contained session is not the live handle")
	}
	client.mu.Unlock()
	client.noteDead(ctx)
	first.mu.Lock()
	if first.closed != 1 {
		first.mu.Unlock()
		t.Fatalf("first closed = %d, want exactly once after drop", first.closed)
	}
	first.mu.Unlock()
	starter.mu.Lock()
	starter.session = second
	starter.mu.Unlock()
	if err := client.ensureConnected(ctx); err != nil {
		t.Fatalf("ensureConnected(): %v", err)
	}
	starter.mu.Lock()
	calls := len(starter.requests)
	starter.mu.Unlock()
	if calls != 2 {
		t.Fatalf("starter calls = %d, want 2", calls)
	}
	first.mu.Lock()
	if first.closed != 1 {
		first.mu.Unlock()
		t.Fatalf("first closed = %d, want exactly once after reconnect", first.closed)
	}
	first.mu.Unlock()
	second.mu.Lock()
	if second.closed != 0 {
		second.mu.Unlock()
		t.Fatal("second session closed unexpectedly")
	}
	second.mu.Unlock()
	client.mu.Lock()
	live := client.containedSession
	session := client.session
	client.mu.Unlock()
	if live != ContainedSession(second) {
		t.Fatal("second contained session is not the live handle")
	}
	if session == nil {
		t.Fatal("sdk session is not connected after reconnect")
	}
}

func TestContainedFailedStartClosesPriorSession(t *testing.T) {
	ctx := t.Context()
	first := &scriptedSession{}
	starter := &stubSessionStarter{session: first}
	client, err := NewClient(containedStdioConfig(), nil, WithSessionStarter(starter))
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	defer func() { _ = client.Close(t.Context()) }()
	if err := client.Connect(ctx, nil); err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	starter.mu.Lock()
	starter.err = errors.New("sandbox unavailable")
	starter.mu.Unlock()
	if err := client.Connect(ctx, nil); err == nil {
		t.Fatal("failed containment accepted")
	}
	first.mu.Lock()
	if first.closed != 1 {
		first.mu.Unlock()
		t.Fatalf("first closed = %d, want exactly once after failed restart", first.closed)
	}
	first.mu.Unlock()
	client.mu.Lock()
	live := client.containedSession
	client.mu.Unlock()
	if live != nil {
		t.Fatal("contained handle not released after failed start")
	}
}

func TestSessionTransportBounds(t *testing.T) {
	transport, err := newSessionTransport(&scriptedSession{}, "s")
	if err != nil {
		t.Fatalf("newSessionTransport(): %v", err)
	}
	if _, err := newSessionTransport(nil, "s"); err == nil {
		t.Error("nil session accepted")
	}
	conn, err := transport.Connect(t.Context())
	if err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	if got := conn.SessionID(); got != "" {
		t.Errorf("session id = %q", got)
	}
	var nilCtx context.Context
	if _, err := conn.Read(nilCtx); err == nil {
		t.Error("nil context accepted")
	}
}
