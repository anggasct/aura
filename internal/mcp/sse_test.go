package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func legacySSEServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "legacy-server", Version: "1.0.0"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "ping", Description: "ping"}, func(_ context.Context, _ *sdk.CallToolRequest, _ EchoInput) (*sdk.CallToolResult, EchoOutput, error) {
		return nil, EchoOutput{Reply: "pong"}, nil
	})
	handler := sdk.NewSSEHandler(func(_ *http.Request) *sdk.Server { return server }, nil)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	return httpServer
}

func TestLegacySSEConnectAndDiscover(t *testing.T) {
	ctx := t.Context()
	httpServer := legacySSEServer(t)
	serverCfg := &config.MCPServer{
		Name: "legacy", Transport: config.MCPTransportLegacySSE, URL: httpServer.URL,
		CompatibilityAck: true,
		RequestTimeout:   config.Duration(10 * time.Second),
		ConnectTimeout:   config.Duration(10 * time.Second),
		StartupTimeout:   config.Duration(10 * time.Second),
		MaxMessageSize:   1 << 20,
	}
	var observations []*Observation
	client, err := NewClient(serverCfg, nil,
		WithHTTPClient(&http.Client{}),
		WithEndpointPolicy(stubEndpointPolicy{}),
		WithObserver(func(_ context.Context, observation *Observation) {
			observations = append(observations, observation)
		}),
	)
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	defer func() { _ = client.Close(t.Context()) }()
	if err := client.Connect(ctx, nil); err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	tools, err := client.DiscoverTools(ctx)
	if err != nil {
		t.Fatalf("DiscoverTools(): %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "ping" {
		t.Fatalf("tools = %+v", tools)
	}
	if len(observations) < 2 {
		t.Fatalf("observations = %d, want connect + discovery", len(observations))
	}
	if observations[0].Transport != config.MCPTransportLegacySSE || observations[0].Result != ResultConnected {
		t.Errorf("connect observation = %+v", observations[0])
	}
}

func TestLegacySSEOAuthFailsClosed(t *testing.T) {
	ctx := t.Context()
	httpServer := legacySSEServer(t)
	serverCfg := &config.MCPServer{
		Name: "legacy-oauth", Transport: config.MCPTransportLegacySSE, URL: httpServer.URL,
		CompatibilityAck: true,
		Auth:             &config.MCPAuth{OAuth: &config.MCPOAuth{ClientIDEnv: "X", ClientSecretEnv: "Y"}},
		RequestTimeout:   config.Duration(10 * time.Second),
	}
	client, err := NewClient(serverCfg, nil,
		WithHTTPClient(&http.Client{}),
		WithEndpointPolicy(stubEndpointPolicy{}),
	)
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	defer func() { _ = client.Close(t.Context()) }()
	if err := client.Connect(ctx, nil); err == nil {
		t.Fatal("oauth over legacy SSE accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrCapabilityUnavailable {
		t.Fatalf("code = %v, %v", code, ok)
	}
}

func TestLegacySSECheckerEmitsFinding(t *testing.T) {
	servers := []config.MCPServer{
		{Name: "legacy-one", Transport: config.MCPTransportLegacySSE},
		{Name: "modern", Transport: config.MCPTransportStreamableHTTP},
	}
	checker := NewLegacySSEChecker(servers)
	findings := checker.Check(t.Context())
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(findings))
	}
	finding := findings[0]
	if finding.Code != "mcp_legacy_sse" || finding.Status != health.StatusDegraded || finding.Severity != health.SeverityWarning {
		t.Errorf("finding = %+v", finding)
	}
	if finding.Scope != "legacy-one" {
		t.Errorf("scope = %q", finding.Scope)
	}
	if findings := checker.Check(t.Context()); len(findings) != 1 {
		t.Errorf("checker not deterministic: %d", len(findings))
	}
	var nilCtx context.Context
	if findings := NewLegacySSEChecker(nil).Check(nilCtx); len(findings) != 0 {
		t.Errorf("nil context findings = %d", len(findings))
	}
}

func TestLegacyHandshakeCancelCarriesStableCode(t *testing.T) {
	blocking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(blocking.Close)
	serverCfg := &config.MCPServer{
		Name:             "legacy-cancel",
		Transport:        config.MCPTransportLegacySSE,
		URL:              blocking.URL,
		CompatibilityAck: true,
		RequestTimeout:   config.Duration(10 * time.Second),
		ConnectTimeout:   config.Duration(10 * time.Second),
		StartupTimeout:   config.Duration(10 * time.Second),
		MaxMessageSize:   1 << 20,
	}
	client, err := NewClient(serverCfg, nil,
		WithHTTPClient(&http.Client{}),
		WithEndpointPolicy(stubEndpointPolicy{}),
	)
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	defer func() { _ = client.Close(t.Context()) }()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	err = client.Connect(ctx, nil)
	if err == nil {
		t.Fatal("cancelled legacy handshake accepted")
	}
	code, ok := CodeOf(err)
	if !ok {
		t.Fatalf("cancelled handshake error carries no code: %v", err)
	}
	if code != ErrServerUnavailable {
		t.Fatalf("code = %q, want %q (err: %v)", code, ErrServerUnavailable, err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled handshake error does not match context.Canceled: %v", err)
	}
}
