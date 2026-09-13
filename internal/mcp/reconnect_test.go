package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type droppingServer struct {
	t       *testing.T
	drop    atomic.Bool
	invoked atomic.Int64
	server  *httptest.Server
}

func newDroppingServer(t *testing.T) *droppingServer {
	t.Helper()
	dropped := &droppingServer{t: t}
	mcpServer := sdk.NewServer(&sdk.Implementation{Name: "drop-server", Version: "1.0.0"}, nil)
	sdk.AddTool(mcpServer, &sdk.Tool{Name: "work", Description: "work"}, func(_ context.Context, _ *sdk.CallToolRequest, in EchoInput) (*sdk.CallToolResult, EchoOutput, error) {
		dropped.invoked.Add(1)
		return nil, EchoOutput{Reply: in.Message}, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(_ *http.Request) *sdk.Server { return mcpServer }, nil)
	mux := http.NewServeMux()
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if dropped.drop.Load() {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("hijack unsupported")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		handler.ServeHTTP(w, r)
	}))
	dropped.server = httptest.NewServer(mux)
	t.Cleanup(dropped.server.Close)
	return dropped
}

func reconnectTestConfig(name, serverURL string) *config.MCPServer {
	return &config.MCPServer{
		Name: name, Transport: config.MCPTransportStreamableHTTP, URL: serverURL,
		Restart:        &config.MCPRestart{MaxAttempts: 3, Window: config.Duration(time.Minute)},
		RequestTimeout: config.Duration(10 * time.Second),
		ConnectTimeout: config.Duration(10 * time.Second),
		StartupTimeout: config.Duration(10 * time.Second),
		MaxMessageSize: 1 << 20,
	}
}

func callWork(ctx context.Context, t *testing.T, client *Client) error {
	t.Helper()
	args, err := json.Marshal(EchoInput{Message: "job"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	_, err = client.CallTool(ctx, "work", args)
	return err
}

func TestReconnectHealsWithoutDuplicating(t *testing.T) {
	ctx := t.Context()
	dropped := newDroppingServer(t)
	client, err := NewClient(reconnectTestConfig("drop", dropped.server.URL), nil,
		WithHTTPClient(&http.Client{}),
		WithEndpointPolicy(stubEndpointPolicy{}),
	)
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	defer func() { _ = client.Close(t.Context()) }()
	if err := client.Connect(ctx, nil); err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	if err := callWork(ctx, t, client); err != nil {
		t.Fatalf("CallTool(): %v", err)
	}
	if got := dropped.invoked.Load(); got != 1 {
		t.Fatalf("invocations = %d, want 1", got)
	}
	dropped.drop.Store(true)
	if err := callWork(ctx, t, client); err == nil {
		t.Fatal("call during outage accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrServerUnavailable {
		t.Fatalf("code = %v, %v", code, ok)
	}
	if got := dropped.invoked.Load(); got != 1 {
		t.Fatalf("invocations = %d, want failed call to execute zero times", got)
	}
	dropped.drop.Store(false)
	if err := callWork(ctx, t, client); err != nil {
		t.Fatalf("CallTool after heal: %v", err)
	}
	if got := dropped.invoked.Load(); got != 2 {
		t.Fatalf("invocations = %d, want exactly one execution per successful call", got)
	}
}

func TestReconnectBudgetExhausts(t *testing.T) {
	ctx := t.Context()
	dropped := newDroppingServer(t)
	cfg := reconnectTestConfig("budget", dropped.server.URL)
	cfg.Restart.MaxAttempts = 1
	client, err := NewClient(cfg, nil,
		WithHTTPClient(&http.Client{}),
		WithEndpointPolicy(stubEndpointPolicy{}),
	)
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	defer func() { _ = client.Close(t.Context()) }()
	if err := client.Connect(ctx, nil); err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	dropped.drop.Store(true)
	if err := callWork(ctx, t, client); err == nil {
		t.Fatal("call during outage accepted")
	}
	client.mu.Lock()
	afterFailure := len(client.restarts)
	client.mu.Unlock()
	if afterFailure != 0 {
		t.Fatalf("restarts = %d, want failed call to record no attempt", afterFailure)
	}
	if err := callWork(ctx, t, client); err == nil {
		t.Fatal("reconnect during outage accepted")
	}
	client.mu.Lock()
	afterReconnect := len(client.restarts)
	client.mu.Unlock()
	if afterReconnect != 1 {
		t.Fatalf("restarts = %d, want 1 recorded attempt", afterReconnect)
	}
	if err := callWork(ctx, t, client); err == nil {
		t.Fatal("over-budget reconnect accepted")
	}
	client.mu.Lock()
	afterExhausted := len(client.restarts)
	client.mu.Unlock()
	if afterExhausted != 1 {
		t.Errorf("restarts = %d, want exhausted budget to record nothing", afterExhausted)
	}
}

func TestReconnectWindowPrunes(t *testing.T) {
	cfg := &config.MCPServer{Name: "window", Restart: &config.MCPRestart{MaxAttempts: 1, Window: config.Duration(40 * time.Millisecond)}}
	client, err := NewClient(cfg, nil)
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	defer func() { _ = client.Close(t.Context()) }()
	now := time.Now()
	client.mu.Lock()
	if !client.reconnectAllowedLocked(now) {
		t.Fatal("first attempt denied")
	}
	if client.reconnectAllowedLocked(now) {
		t.Fatal("second attempt allowed within budget")
	}
	if !client.reconnectAllowedLocked(now.Add(100 * time.Millisecond)) {
		t.Fatal("attempt denied after window elapsed")
	}
	client.resetReconnectsLocked()
	if !client.reconnectAllowedLocked(now) {
		t.Fatal("attempt denied after reset")
	}
	client.mu.Unlock()
}

func TestReconnectDisabledWithoutPolicy(t *testing.T) {
	cfg := &config.MCPServer{Name: "no-restart"}
	client, err := NewClient(cfg, nil)
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	defer func() { _ = client.Close(t.Context()) }()
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.reconnectAllowedLocked(time.Now()) {
		t.Error("reconnect allowed without restart policy")
	}
}
