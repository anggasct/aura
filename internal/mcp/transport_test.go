package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func staticStreamableConfig(name, serverURL string) *config.MCPServer {
	return &config.MCPServer{
		Name: name, Transport: config.MCPTransportStreamableHTTP, URL: serverURL,
		Auth:           &config.MCPAuth{Static: &config.MCPStaticAuth{CredentialRef: "env://MCP_STATIC_TOKEN"}},
		RequestTimeout: config.Duration(10 * time.Second),
		ConnectTimeout: config.Duration(10 * time.Second),
		StartupTimeout: config.Duration(10 * time.Second),
		MaxMessageSize: 1 << 20,
	}
}

func authedStreamableServer(t *testing.T, expected, toolReply func() string) *httptest.Server {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "static-mcp-server", Version: "1.0.0"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "echo", Description: "echoes message"}, func(_ context.Context, _ *sdk.CallToolRequest, in EchoInput) (*sdk.CallToolResult, EchoOutput, error) {
		return nil, EchoOutput{Reply: toolReply()}, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(_ *http.Request) *sdk.Server { return server }, nil)
	mux := http.NewServeMux()
	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+expected() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)
	return httpServer
}

func TestStaticAuthEndToEndWithRotation(t *testing.T) {
	ctx := t.Context()
	current := "static-secret-v1"
	httpServer := authedStreamableServer(t, func() string { return current }, func() string { return "ok" })
	t.Setenv("MCP_STATIC_TOKEN", "static-secret-v1")
	resolver := &stubSecretResolver{values: map[string]string{"env://MCP_STATIC_TOKEN": "static-secret-v1"}}

	client, err := NewClient(staticStreamableConfig("static-e2e", httpServer.URL+"/mcp"), nil,
		WithHTTPClient(&http.Client{}),
		WithEndpointPolicy(stubEndpointPolicy{}),
		WithSecretResolver(resolver),
	)
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	defer func() { _ = client.Close(t.Context()) }()
	if err := client.Connect(ctx, nil); err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	args, err := json.Marshal(EchoInput{Message: "hi"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := client.CallTool(ctx, "echo", args); err != nil {
		t.Fatalf("CallTool(): %v", err)
	}

	current = "static-secret-v2"
	t.Setenv("MCP_STATIC_TOKEN", "static-secret-v2")
	resolver.values["env://MCP_STATIC_TOKEN"] = "static-secret-v2"
	if _, err := client.CallTool(ctx, "echo", args); err != nil {
		t.Fatalf("CallTool after rotation: %v", err)
	}
	if resolver.calls < 2 {
		t.Errorf("credential resolved %d times, want per-request resolution", resolver.calls)
	}
}

func TestEndpointPolicyDenyBlocksConnect(t *testing.T) {
	ctx := t.Context()
	httpServer := authedStreamableServer(t, func() string { return "x" }, func() string { return "x" })
	policyErr := Errorf(ErrEgressDenied, "test policy denies all")
	client, err := NewClient(staticStreamableConfig("denied", httpServer.URL+"/mcp"), nil,
		WithHTTPClient(&http.Client{}),
		WithEndpointPolicy(stubEndpointPolicy{err: policyErr}),
		WithSecretResolver(&stubSecretResolver{values: map[string]string{"env://MCP_STATIC_TOKEN": "x"}}),
	)
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	defer func() { _ = client.Close(t.Context()) }()
	if err := client.Connect(ctx, nil); err == nil {
		t.Fatal("denied endpoint accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrEgressDenied {
		t.Fatalf("code = %v, %v", code, ok)
	}
}

func TestCrossOriginRedirectDenied(t *testing.T) {
	ctx := t.Context()
	var targetHits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/mcp", http.StatusFound)
	}))
	t.Cleanup(origin.Close)

	allow := true
	serverCfg := staticStreamableConfig("redirected", origin.URL+"/mcp")
	serverCfg.AllowRedirects = &allow
	client, err := NewClient(serverCfg, nil,
		WithHTTPClient(&http.Client{}),
		WithEndpointPolicy(stubEndpointPolicy{}),
		WithSecretResolver(&stubSecretResolver{values: map[string]string{"env://MCP_STATIC_TOKEN": "x"}}),
	)
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	defer func() { _ = client.Close(t.Context()) }()
	if err := client.Connect(ctx, nil); err == nil {
		t.Fatal("cross-origin redirect accepted")
	}
	if targetHits != 0 {
		t.Errorf("redirect target hit %d times", targetHits)
	}
}

func TestSameOriginRedirectAllowed(t *testing.T) {
	ctx := t.Context()
	server := sdk.NewServer(&sdk.Implementation{Name: "redir-server", Version: "1.0.0"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "ping", Description: "ping"}, func(_ context.Context, _ *sdk.CallToolRequest, _ EchoInput) (*sdk.CallToolResult, EchoOutput, error) {
		return nil, EchoOutput{Reply: "pong"}, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(_ *http.Request) *sdk.Server { return server }, nil)
	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	mux.Handle("/entry", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/mcp", http.StatusTemporaryRedirect)
	}))
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	allow := true
	serverCfg := &config.MCPServer{
		Name: "same-origin", Transport: config.MCPTransportStreamableHTTP, URL: httpServer.URL + "/entry",
		AllowRedirects: &allow,
		RequestTimeout: config.Duration(10 * time.Second),
		ConnectTimeout: config.Duration(10 * time.Second),
		StartupTimeout: config.Duration(10 * time.Second),
		MaxMessageSize: 1 << 20,
	}
	client, err := NewClient(serverCfg, nil,
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
	tools, err := client.DiscoverTools(ctx)
	if err != nil {
		t.Fatalf("DiscoverTools(): %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "ping" {
		t.Fatalf("tools = %+v", tools)
	}
}

func TestOversizeToolResultFails(t *testing.T) {
	ctx := t.Context()
	server := sdk.NewServer(&sdk.Implementation{Name: "big-server", Version: "1.0.0"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "big", Description: "big"}, func(_ context.Context, _ *sdk.CallToolRequest, _ EchoInput) (*sdk.CallToolResult, EchoOutput, error) {
		return nil, EchoOutput{Reply: strings.Repeat("z", 64*1024)}, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(_ *http.Request) *sdk.Server { return server }, nil)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	serverCfg := &config.MCPServer{
		Name: "big", Transport: config.MCPTransportStreamableHTTP, URL: httpServer.URL,
		RequestTimeout: config.Duration(10 * time.Second),
		ConnectTimeout: config.Duration(10 * time.Second),
		StartupTimeout: config.Duration(10 * time.Second),
		MaxMessageSize: 1024,
	}
	client, err := NewClient(serverCfg, nil,
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
	args, err := json.Marshal(EchoInput{Message: "x"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := client.CallTool(ctx, "big", args); err == nil {
		t.Fatal("oversize result accepted")
	}
}
