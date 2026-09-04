package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
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

	serverCfg := &config.MCPServer{
		Name:           "mem-server",
		Transport:      config.MCPTransportStdio,
		RequestTimeout: config.Duration(5 * time.Second),
		StartupTimeout: config.Duration(5 * time.Second),
	}

	client, err := NewClient(serverCfg, nil)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer func() { _ = client.Close() }()

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

	serverCfg := &config.MCPServer{
		Name:      "no-tools-server",
		Transport: config.MCPTransportStdio,
	}

	client, err := NewClient(serverCfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	err = client.Connect(ctx, clientTransport)
	if err == nil {
		t.Fatal("expected connect to fail due to missing tools capability")
	}
	if code, ok := CodeOf(err); !ok || code != ErrCapabilityUnavailable {
		t.Fatalf("expected %s, got %s (err: %v)", ErrCapabilityUnavailable, code, err)
	}
}

func TestClientNotConnectedErrors(t *testing.T) {
	ctx := t.Context()
	serverCfg := &config.MCPServer{
		Name:      "unconnected",
		Transport: config.MCPTransportStdio,
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
		if code, _ := CodeOf(err); code != ErrServerUnavailable {
			t.Fatalf("expected %s, got %s", ErrServerUnavailable, code)
		}
	})

	t.Run("call tool without connect", func(t *testing.T) {
		_, err := client.CallTool(ctx, "echo", nil)
		if err == nil {
			t.Fatal("expected error")
		}
		if code, _ := CodeOf(err); code != ErrServerUnavailable {
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

	serverCfg := &config.MCPServer{
		Name:           "local-stdio",
		Transport:      config.MCPTransportStdio,
		Command:        os.Args[0],
		Args:           []string{"-test.run=TestHelperProcess", "--"},
		Environment:    map[string]string{"GO_WANT_HELPER_PROCESS": "1"},
		StartupTimeout: config.Duration(10 * time.Second),
		RequestTimeout: config.Duration(10 * time.Second),
		MaxMessageSize: 1024 * 1024,
	}

	client, err := NewClient(serverCfg, nil)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Connect(ctx, nil); err != nil {
		t.Fatalf("Connect over stdio failed: %v", err)
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

	serverCfg := &config.MCPServer{
		Name:           "ancient-server",
		Transport:      config.MCPTransportStdio,
		StartupTimeout: config.Duration(3 * time.Second),
		RequestTimeout: config.Duration(3 * time.Second),
	}

	client, err := NewClient(serverCfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	err = client.Connect(ctx, clientTransport)
	if err == nil {
		t.Fatal("expected Connect to fail with unsupported protocol version")
	}
	if code, ok := CodeOf(err); !ok || code != ErrProtocolUnsupported {
		t.Fatalf("expected %s, got %s (err: %v)", ErrProtocolUnsupported, code, err)
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

		serverCfg := &config.MCPServer{
			Name:           "dup-server",
			Transport:      config.MCPTransportStdio,
			StartupTimeout: config.Duration(3 * time.Second),
			RequestTimeout: config.Duration(3 * time.Second),
		}
		client, err := NewClient(serverCfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = client.Close() }()

		if err := client.Connect(ctx, clientTransport); err != nil {
			t.Fatalf("Connect failed: %v", err)
		}

		_, err = client.DiscoverTools(ctx)
		if err == nil {
			t.Fatal("expected duplicate tool names to fail")
		}
		if code, ok := CodeOf(err); !ok || code != ErrSchemaInvalid {
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

		serverCfg := &config.MCPServer{
			Name:           "large-server",
			Transport:      config.MCPTransportStdio,
			MaxMessageSize: 30,
			StartupTimeout: config.Duration(3 * time.Second),
			RequestTimeout: config.Duration(3 * time.Second),
		}
		client, err := NewClient(serverCfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = client.Close() }()

		if err := client.Connect(ctx, clientTransport); err != nil {
			t.Fatalf("Connect failed: %v", err)
		}

		_, err = client.DiscoverTools(ctx)
		if err == nil {
			t.Fatal("expected oversized schema to fail")
		}
		if code, ok := CodeOf(err); !ok || code != ErrMessageTooLarge {
			t.Fatalf("expected %s, got %s", ErrMessageTooLarge, code)
		}
	})
}
