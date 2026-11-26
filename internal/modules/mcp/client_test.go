package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type EchoInput struct {
	Message string `json:"message"`
}

type EchoOutput struct {
	Reply string `json:"reply"`
}

func setupTestServer(t *testing.T, addTools bool) *sdk.Server {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{
		Name:    "test-server",
		Version: "1.0.0",
	}, nil)

	if addTools {
		sdk.AddTool(server, &sdk.Tool{
			Name:        "echo",
			Description: "echoes message",
		}, func(_ context.Context, _ *sdk.CallToolRequest, in EchoInput) (*sdk.CallToolResult, EchoOutput, error) {
			return nil, EchoOutput{Reply: in.Message}, nil
		})
	}
	return server
}

func TestClientInMemory(t *testing.T) {
	ctx := t.Context()
	server := setupTestServer(t, true)

	clientTransport, serverTransport := sdk.NewInMemoryTransports()
	go func() {
		_ = server.Run(ctx, serverTransport)
	}()

	serverCfg := &ServerConfig{
		Name:           "mem-server",
		Transport:      TransportStdio,
		RequestTimeout: configkit.Duration(5 * time.Second),
		StartupTimeout: configkit.Duration(5 * time.Second),
		MaxMessageSize: DefaultMaxMessageSize,
	}

	client, err := NewClient(serverCfg, nil)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer func() { _ = client.Close(ctx) }()

	if err := client.Connect(ctx, clientTransport); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	tools, err := client.DiscoverTools(ctx)
	if err != nil {
		t.Fatalf("DiscoverTools failed: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("unexpected tools: %+v", tools)
	}

	args, err := json.Marshal(EchoInput{Message: "hello world"})
	if err != nil {
		t.Fatal(err)
	}

	res, err := client.CallTool(ctx, "echo", args)
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error result")
	}
	if res.StructuredContent == nil {
		t.Fatal("expected structured content in result")
	}
}

func TestClientMissingToolCapability(t *testing.T) {
	ctx := t.Context()
	server := setupTestServer(t, false) // No tools added, so server has no tools capability

	clientTransport, serverTransport := sdk.NewInMemoryTransports()
	go func() {
		_ = server.Run(ctx, serverTransport)
	}()

	serverCfg := &ServerConfig{
		Name:           "no-tools-server",
		Transport:      TransportStdio,
		StartupTimeout: DefaultStartupTimeout,
		RequestTimeout: DefaultRequestTimeout,
		MaxMessageSize: DefaultMaxMessageSize,
	}

	client, err := NewClient(serverCfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(ctx) }()

	err = client.Connect(ctx, clientTransport)
	if err == nil {
		t.Fatal("expected connect to fail due to missing tools capability")
	}
	if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrCapabilityUnavailable {
		t.Fatalf("expected %s, got %s (err: %v)", ErrCapabilityUnavailable, code, err)
	}
}

func TestClientNotConnectedErrors(t *testing.T) {
	ctx := t.Context()
	serverCfg := &ServerConfig{
		Name:           "unconnected",
		Transport:      TransportStdio,
		StartupTimeout: DefaultStartupTimeout,
		RequestTimeout: DefaultRequestTimeout,
		MaxMessageSize: DefaultMaxMessageSize,
	}

	client, err := NewClient(serverCfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("discover without connect", func(t *testing.T) {
		_, err := client.DiscoverTools(ctx)
		if err == nil {
			t.Fatal("expected error")
		}
		if code, _ := fault.CodeOf[ErrorCode](err); code != ErrServerUnavailable {
			t.Fatalf("expected %s, got %s", ErrServerUnavailable, code)
		}
	})

	t.Run("call tool without connect", func(t *testing.T) {
		_, err := client.CallTool(ctx, "echo", nil)
		if err == nil {
			t.Fatal("expected error")
		}
		if code, _ := fault.CodeOf[ErrorCode](err); code != ErrServerUnavailable {
			t.Fatalf("expected %s, got %s", ErrServerUnavailable, code)
		}
	})
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	server := sdk.NewServer(&sdk.Implementation{
		Name:    "stdio-helper-server",
		Version: "1.0.0",
	}, nil)

	sdk.AddTool(server, &sdk.Tool{
		Name:        "ping",
		Description: "ping tool",
	}, func(_ context.Context, _ *sdk.CallToolRequest, _ EchoInput) (*sdk.CallToolResult, EchoOutput, error) {
		return nil, EchoOutput{Reply: "pong"}, nil
	})

	_ = server.Run(context.Background(), &sdk.StdioTransport{})
	os.Exit(0)
}

func TestClientStdioTransport(t *testing.T) {
	ctx := t.Context()

	serverCfg := &ServerConfig{
		Name:           "local-stdio",
		Transport:      TransportStdio,
		Command:        "/bin/contained-stdio",
		Environment:    map[string]string{"SERVER_MODE": "test"},
		StartupTimeout: configkit.Duration(10 * time.Second),
		RequestTimeout: configkit.Duration(10 * time.Second),
		MaxMessageSize: 1024 * 1024,
	}

	starter := &stubSessionStarter{session: &scriptedSession{toolNames: []string{"ping"}}}
	client, err := NewClient(serverCfg, nil, WithSessionStarter(starter))
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer func() { _ = client.Close(ctx) }()

	if err := client.Connect(ctx, nil); err != nil {
		t.Fatalf("Connect over contained stdio failed: %v", err)
	}

	tools, err := client.DiscoverTools(ctx)
	if err != nil {
		t.Fatalf("DiscoverTools over stdio failed: %v", err)
	}

	if len(tools) != 1 || tools[0].Name != "ping" {
		t.Fatalf("expected ping tool, got %+v", tools)
	}

	callArgs, err := json.Marshal(EchoInput{Message: "ping"})
	if err != nil {
		t.Fatalf("marshal ping input failed: %v", err)
	}
	res, err := client.CallTool(ctx, "ping", callArgs)
	if err != nil {
		t.Fatalf("CallTool over stdio failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error result")
	}
}

func TestClientUnsupportedProtocolVersion(t *testing.T) {
	ctx := t.Context()

	server := sdk.NewServer(&sdk.Implementation{Name: "ancient-server", Version: "1.0"}, nil)
	server.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			if method == "server/discover" || method == "initialize" {
				return nil, errors.New("unsupported protocol version: 2020-01-01")
			}
			return next(ctx, method, req)
		}
	})

	clientTransport, serverTransport := sdk.NewInMemoryTransports()
	go func() {
		_ = server.Run(ctx, serverTransport)
	}()

	serverCfg := &ServerConfig{
		Name:           "ancient-server",
		Transport:      TransportStdio,
		StartupTimeout: configkit.Duration(3 * time.Second),
		RequestTimeout: configkit.Duration(3 * time.Second),
		MaxMessageSize: DefaultMaxMessageSize,
	}

	client, err := NewClient(serverCfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(ctx) }()

	err = client.Connect(ctx, clientTransport)
	if err == nil {
		t.Fatal("expected Connect to fail for handshake failure")
	}
	if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrServerUnavailable {
		t.Fatalf("expected %s, got %s (err: %v)", ErrServerUnavailable, code, err)
	}
}

func TestExplicitProtocolVersionGate(t *testing.T) {
	for _, v := range SupportedProtocolVersions {
		if !IsSupportedProtocolVersion(v) {
			t.Fatalf("expected version %q to be supported", v)
		}
	}
	if IsSupportedProtocolVersion("2020-01-01") {
		t.Fatal("expected version 2020-01-01 to be unsupported")
	}
	if IsSupportedProtocolVersion("") {
		t.Fatal("expected empty version to be unsupported")
	}
}

func TestClientDiscoveryValidation(t *testing.T) {
	ctx := t.Context()

	t.Run("duplicate tool names fail with schema invalid", func(t *testing.T) {
		server := sdk.NewServer(&sdk.Implementation{Name: "dup-server", Version: "1.0"}, nil)
		server.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
			return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
				if method == "tools/list" {
					return &sdk.ListToolsResult{
						Tools: []*sdk.Tool{
							{Name: "echo", InputSchema: map[string]any{"type": "object"}},
							{Name: "echo", InputSchema: map[string]any{"type": "object"}},
						},
					}, nil
				}
				return next(ctx, method, req)
			}
		})
		sdk.AddTool(server, &sdk.Tool{Name: "placeholder"}, func(_ context.Context, _ *sdk.CallToolRequest, in EchoInput) (*sdk.CallToolResult, EchoOutput, error) {
			return nil, EchoOutput{}, nil
		})

		clientTransport, serverTransport := sdk.NewInMemoryTransports()
		go func() {
			_ = server.Run(ctx, serverTransport)
		}()

		serverCfg := &ServerConfig{
			Name:           "dup-server",
			Transport:      TransportStdio,
			StartupTimeout: configkit.Duration(3 * time.Second),
			RequestTimeout: configkit.Duration(3 * time.Second),
			MaxMessageSize: DefaultMaxMessageSize,
		}
		client, err := NewClient(serverCfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = client.Close(ctx) }()

		if err := client.Connect(ctx, clientTransport); err != nil {
			t.Fatalf("Connect failed: %v", err)
		}

		_, err = client.DiscoverTools(ctx)
		if err == nil {
			t.Fatal("expected duplicate tool names to fail")
		}
		if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrSchemaInvalid {
			t.Fatalf("expected %s, got %s", ErrSchemaInvalid, code)
		}
	})

	t.Run("oversized schema fails with message too large", func(t *testing.T) {
		server := sdk.NewServer(&sdk.Implementation{Name: "large-server", Version: "1.0"}, nil)
		server.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
			return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
				if method == "tools/list" {
					return &sdk.ListToolsResult{
						Tools: []*sdk.Tool{
							{Name: "huge", InputSchema: map[string]any{"type": "object", "description": "oversized description that exceeds the small message size limit"}},
						},
					}, nil
				}
				return next(ctx, method, req)
			}
		})
		sdk.AddTool(server, &sdk.Tool{Name: "placeholder"}, func(_ context.Context, _ *sdk.CallToolRequest, in EchoInput) (*sdk.CallToolResult, EchoOutput, error) {
			return nil, EchoOutput{}, nil
		})

		clientTransport, serverTransport := sdk.NewInMemoryTransports()
		go func() {
			_ = server.Run(ctx, serverTransport)
		}()

		serverCfg := &ServerConfig{
			Name:           "large-server",
			Transport:      TransportStdio,
			MaxMessageSize: 30,
			StartupTimeout: configkit.Duration(3 * time.Second),
			RequestTimeout: configkit.Duration(3 * time.Second),
		}
		client, err := NewClient(serverCfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = client.Close(ctx) }()

		if err := client.Connect(ctx, clientTransport); err != nil {
			t.Fatalf("Connect failed: %v", err)
		}

		_, err = client.DiscoverTools(ctx)
		if err == nil {
			t.Fatal("expected oversized schema to fail")
		}
		if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrMessageTooLarge {
			t.Fatalf("expected %s, got %s", ErrMessageTooLarge, code)
		}
	})
}

func TestClientStreamableHTTPTransport(t *testing.T) {
	ctx := t.Context()
	server := sdk.NewServer(&sdk.Implementation{Name: "http-server", Version: "1.0.0"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "echo", Description: "echoes message"}, func(_ context.Context, _ *sdk.CallToolRequest, in EchoInput) (*sdk.CallToolResult, EchoOutput, error) {
		return nil, EchoOutput{Reply: in.Message}, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(_ *http.Request) *sdk.Server { return server }, nil)
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	serverCfg := &ServerConfig{
		Name:           "http-server",
		Transport:      TransportStreamableHTTP,
		URL:            httpServer.URL,
		RequestTimeout: configkit.Duration(5 * time.Second),
		ConnectTimeout: durationPtr(5 * time.Second),
		StartupTimeout: configkit.Duration(5 * time.Second),
		MaxMessageSize: 1024 * 1024,
	}
	client, err := NewClient(serverCfg, nil,
		WithHTTPClient(&http.Client{}),
		WithEndpointPolicy(stubEndpointPolicy{}),
	)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer func() { _ = client.Close(ctx) }()

	if err := client.Connect(ctx, nil); err != nil {
		t.Fatalf("Connect over streamable HTTP failed: %v", err)
	}
	tools, err := client.DiscoverTools(ctx)
	if err != nil {
		t.Fatalf("DiscoverTools over streamable HTTP failed: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("expected echo tool, got %+v", tools)
	}
}

func TestClientStreamableHTTPRequiresAuth(t *testing.T) {
	ctx := t.Context()
	serverCfg := &ServerConfig{
		Name:           "auth-server",
		Transport:      TransportStreamableHTTP,
		URL:            "http://127.0.0.1:9/mcp",
		StartupTimeout: DefaultStartupTimeout,
		RequestTimeout: DefaultRequestTimeout,
		MaxMessageSize: DefaultMaxMessageSize,
		Auth: &AuthConfig{
			Static: &StaticAuthConfig{CredentialRef: mustSecretRef(t, "env://MCP_MISSING_KEY")},
		},
	}
	client, err := NewClient(serverCfg, nil,
		WithHTTPClient(&http.Client{}),
		WithEndpointPolicy(stubEndpointPolicy{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(ctx) }()
	if err := client.Connect(ctx, nil); err == nil {
		t.Fatal("expected unresolvable credential to fail")
	} else if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrAuthRequired {
		t.Fatalf("expected %s, got %s (%v)", ErrAuthRequired, code, err)
	}
}

func TestClientStreamableHTTPWithoutClientFailsClosed(t *testing.T) {
	ctx := t.Context()
	serverCfg := &ServerConfig{
		Name:           "bare-server",
		Transport:      TransportStreamableHTTP,
		URL:            "http://127.0.0.1:9/mcp",
		StartupTimeout: DefaultStartupTimeout,
		RequestTimeout: configkit.Duration(5 * time.Second),
		MaxMessageSize: DefaultMaxMessageSize,
	}
	client, err := NewClient(serverCfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(ctx) }()
	if err := client.Connect(ctx, nil); err == nil {
		t.Fatal("expected missing http client to fail closed")
	} else if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrEgressDenied {
		t.Fatalf("expected %s, got %s (%v)", ErrEgressDenied, code, err)
	}
}

func TestClientStreamableHTTPWithoutPolicyFailsClosed(t *testing.T) {
	ctx := t.Context()
	serverCfg := &ServerConfig{
		Name:           "no-policy-server",
		Transport:      TransportStreamableHTTP,
		URL:            "http://127.0.0.1:9/mcp",
		StartupTimeout: DefaultStartupTimeout,
		RequestTimeout: configkit.Duration(5 * time.Second),
		MaxMessageSize: DefaultMaxMessageSize,
	}
	client, err := NewClient(serverCfg, nil, WithHTTPClient(&http.Client{}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(ctx) }()
	if err := client.Connect(ctx, nil); err == nil {
		t.Fatal("expected missing endpoint policy to fail closed")
	} else if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrEgressDenied {
		t.Fatalf("expected %s, got %s (%v)", ErrEgressDenied, code, err)
	}
}

func TestClientDiscoveryNameValidation(t *testing.T) {
	ctx := t.Context()
	cases := []struct {
		name     string
		toolName string
	}{
		{"overlong", strings.Repeat("a", 65)},
		{"illegal_charset", "bad/name"},
		{"control_char", "bad\x00tool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := sdk.NewServer(&sdk.Implementation{Name: "bad-server", Version: "1.0"}, nil)
			toolName := tc.toolName
			server.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
				return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
					if method == "tools/list" {
						return &sdk.ListToolsResult{
							Tools: []*sdk.Tool{{Name: toolName, InputSchema: map[string]any{"type": "object"}}},
						}, nil
					}
					return next(ctx, method, req)
				}
			})
			sdk.AddTool(server, &sdk.Tool{Name: "placeholder"}, func(_ context.Context, _ *sdk.CallToolRequest, in EchoInput) (*sdk.CallToolResult, EchoOutput, error) {
				return nil, EchoOutput{}, nil
			})
			clientTransport, serverTransport := sdk.NewInMemoryTransports()
			go func() { _ = server.Run(ctx, serverTransport) }()
			serverCfg := &ServerConfig{
				Name:           "bad-server",
				Transport:      TransportStdio,
				StartupTimeout: configkit.Duration(3 * time.Second),
				RequestTimeout: configkit.Duration(3 * time.Second),
				MaxMessageSize: DefaultMaxMessageSize,
			}
			client, err := NewClient(serverCfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close(ctx) }()
			if err := client.Connect(ctx, clientTransport); err != nil {
				t.Fatalf("Connect failed: %v", err)
			}
			if _, err := client.DiscoverTools(ctx); err == nil {
				t.Fatalf("expected %s to fail", tc.name)
			} else if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrSchemaInvalid {
				t.Fatalf("expected %s, got %s (%v)", ErrSchemaInvalid, code, err)
			}
		})
	}
}

func TestNewClientRejectsZeroRequestTimeout(t *testing.T) {
	base := func() *ServerConfig {
		return &ServerConfig{
			Name:           "bounded",
			Transport:      TransportStdio,
			Command:        "/usr/local/bin/docs-mcp",
			StartupTimeout: DefaultStartupTimeout,
			RequestTimeout: DefaultRequestTimeout,
			MaxMessageSize: DefaultMaxMessageSize,
		}
	}
	cases := []struct {
		name   string
		mutate func(*ServerConfig)
	}{
		{"zero request_timeout", func(s *ServerConfig) { s.RequestTimeout = 0 }},
		{"zero startup_timeout", func(s *ServerConfig) { s.StartupTimeout = 0 }},
		{"zero max_message_size", func(s *ServerConfig) { s.MaxMessageSize = 0 }},
		{"zero connect_timeout", func(s *ServerConfig) {
			s.Transport = TransportStreamableHTTP
			s.URL = "https://mcp.example.com/mcp"
			s.ConnectTimeout = durationPtr(0)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mutate(cfg)
			if _, err := NewClient(cfg, nil); !fault.HasCode(err, ErrConfigInvalid) {
				t.Fatalf("NewClient(%s) = %v, want %s", tc.name, err, ErrConfigInvalid)
			}
		})
	}
}
