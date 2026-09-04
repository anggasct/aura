package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/toolbroker"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestManagerLifecycleAndToolBrokerIntegration(t *testing.T) {
	ctx := t.Context()

	// MCP Server with echo tool
	server := sdk.NewServer(&sdk.Implementation{
		Name:    "mem-server",
		Version: "1.0.0",
	}, nil)

	sdk.AddTool(server, &sdk.Tool{
		Name:        "echo",
		Description: "echoes message",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in EchoInput) (*sdk.CallToolResult, EchoOutput, error) {
		return nil, EchoOutput{Reply: "ack: " + in.Message}, nil
	})

	clientTransport, serverTransport := sdk.NewInMemoryTransports()
	go func() {
		_ = server.Run(ctx, serverTransport)
	}()

	broker, err := toolbroker.New(&toolbroker.Options{})
	if err != nil {
		t.Fatalf("toolbroker.New failed: %v", err)
	}

	serverName := "echo-server"
	mcpCfg := &config.MCP{
		Servers: []config.MCPServer{
			{
				Name:           serverName,
				Transport:      config.MCPTransportStdio,
				Capabilities:   []string{"workspace-read"},
				RequestTimeout: config.Duration(5 * time.Second),
				StartupTimeout: config.Duration(5 * time.Second),
				MaxMessageSize: 1024 * 1024,
			},
		},
	}

	trustRegistry := NewMemoryTrustRegistry()

	mgr, err := NewManager(ManagerOptions{
		Config:        mcpCfg,
		Broker:        broker,
		TrustRegistry: trustRegistry,
	})
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	mgr.SetCustomTransport(serverName, clientTransport)

	// 1. Initial start fails because trust is required before tools are registered
	err = mgr.Start(ctx)
	if err == nil {
		t.Fatal("expected Start to fail due to trust required")
	}
	if code, ok := CodeOf(err); !ok || code != ErrTrustRequired {
		t.Fatalf("expected %s, got %s (err: %v)", ErrTrustRequired, code, err)
	}

	// Verify no tools were registered in broker
	namespacedTool := FormatToolName(serverName, "echo")
	for _, def := range broker.Definitions() {
		if def.Name == namespacedTool {
			t.Fatalf("tool %s should not be registered while untrusted", namespacedTool)
		}
	}

	// 2. Approve the trust digest recorded during the attempt
	trustRecord, err := trustRegistry.GetTrust(ctx, serverName)
	if err != nil || trustRecord == nil {
		t.Fatalf("expected trust record to exist: %v", err)
	}
	if err := trustRegistry.Approve(ctx, serverName, trustRecord.Digest); err != nil {
		t.Fatalf("Approve failed: %v", err)
	}

	// Create fresh in-memory transports for the reconnect
	clientTransport2, serverTransport2 := sdk.NewInMemoryTransports()
	go func() {
		_ = server.Run(ctx, serverTransport2)
	}()
	mgr.SetCustomTransport(serverName, clientTransport2)

	// 3. Now start succeeds and registers tool
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start failed after approval: %v", err)
	}

	var foundDef bool
	for _, def := range broker.Definitions() {
		if def.Name == namespacedTool {
			foundDef = true
			break
		}
	}
	if !foundDef {
		t.Fatalf("tool %s was not registered in broker", namespacedTool)
	}

	// 4. Invocations pass through ToolBroker with exact server/tool/argument/capability identity and Untrusted: true
	reqArgs, err := json.Marshal(EchoInput{Message: "hello aura"})
	if err != nil {
		t.Fatalf("marshal reqArgs failed: %v", err)
	}
	toolReq := &toolbroker.ToolRequest{
		RequestID:    "req-1",
		TurnID:       "turn-1",
		SessionID:    "session-1",
		PrincipalID:  "principal-1",
		ToolName:     namespacedTool,
		ToolVersion:  "v1",
		Arguments:    reqArgs,
		Capabilities: []string{"workspace-read"},
		Trust:        approval.TrustOwnerInput,
	}

	res, err := broker.Execute(ctx, toolReq)
	if err != nil {
		t.Fatalf("broker.Execute failed: %v", err)
	}
	if !res.Untrusted {
		t.Fatal("expected result to be marked untrusted")
	}
	if res.Class != toolbroker.ResultOK {
		t.Fatalf("expected ResultOK, got %s", res.Class)
	}

	// Missing capability fails closed in broker
	toolReqNoCaps := *toolReq
	toolReqNoCaps.Capabilities = nil
	_, err = broker.Execute(ctx, &toolReqNoCaps)
	if err == nil {
		t.Fatal("expected execution to fail closed when required capabilities are missing")
	}

	// 5. Close unregisters tools
	if err := mgr.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	for _, def := range broker.Definitions() {
		if def.Name == namespacedTool {
			t.Fatalf("tool %s should be unregistered after Close", namespacedTool)
		}
	}
}

func TestManagerStdioEndToEnd(t *testing.T) {
	ctx := t.Context()

	broker, err := toolbroker.New(&toolbroker.Options{})
	if err != nil {
		t.Fatalf("toolbroker.New failed: %v", err)
	}

	serverName := "stdio-e2e"
	serverCfg := config.MCPServer{
		Name:           serverName,
		Transport:      config.MCPTransportStdio,
		Command:        os.Args[0],
		Args:           []string{"-test.run=TestHelperProcess", "--"},
		Environment:    map[string]string{"GO_WANT_HELPER_PROCESS": "1"},
		StartupTimeout: config.Duration(10 * time.Second),
		RequestTimeout: config.Duration(10 * time.Second),
		MaxMessageSize: 1024 * 1024,
	}

	mcpCfg := &config.MCP{
		Servers: []config.MCPServer{serverCfg},
	}

	trustRegistry := NewMemoryTrustRegistry()

	mgr, err := NewManager(ManagerOptions{
		Config:        mcpCfg,
		Broker:        broker,
		TrustRegistry: trustRegistry,
	})
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	// First start records pending trust and returns ErrTrustRequired
	err = mgr.Start(ctx)
	if err == nil {
		t.Fatal("expected ErrTrustRequired on first start without approval")
	}
	if code, _ := CodeOf(err); code != ErrTrustRequired {
		t.Fatalf("expected ErrTrustRequired, got %s", code)
	}

	rec, err := trustRegistry.GetTrust(ctx, serverName)
	if err != nil || rec == nil {
		t.Fatalf("expected trust record to exist: %v", err)
	}
	if err := trustRegistry.Approve(ctx, serverName, rec.Digest); err != nil {
		t.Fatalf("Approve failed: %v", err)
	}

	// Second start with approved digest succeeds
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	namespacedTool := FormatToolName(serverName, "ping")
	callArgs, err := json.Marshal(EchoInput{Message: "hello"})
	if err != nil {
		t.Fatalf("marshal callArgs failed: %v", err)
	}
	toolReq := &toolbroker.ToolRequest{
		RequestID:   "req-2",
		TurnID:      "turn-1",
		SessionID:   "session-1",
		PrincipalID: "principal-1",
		ToolName:    namespacedTool,
		ToolVersion: "v1",
		Arguments:   callArgs,
		Trust:       approval.TrustOwnerInput,
	}

	res, err := broker.Execute(ctx, toolReq)
	if err != nil {
		t.Fatalf("broker.Execute over stdio failed: %v", err)
	}
	if !res.Untrusted {
		t.Fatal("expected stdio tool result to be marked untrusted")
	}
	if res.Class != toolbroker.ResultOK {
		t.Fatalf("expected ResultOK, got %s", res.Class)
	}
}

func TestManagerCapabilityCheckFailure(t *testing.T) {
	ctx := t.Context()
	broker, err := toolbroker.New(&toolbroker.Options{})
	if err != nil {
		t.Fatal(err)
	}

	serverCfg := config.MCPServer{
		Name:         "unsupported-cap-server",
		Transport:    config.MCPTransportStdio,
		Capabilities: []string{"restricted-exec"},
	}

	mcpCfg := &config.MCP{
		Servers: []config.MCPServer{serverCfg},
	}

	mgr, err := NewManager(ManagerOptions{
		Config: mcpCfg,
		Broker: broker,
		CapabilityChecker: func(caps []string) error {
			return errors.New("restricted-exec is not enabled on this profile")
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	err = mgr.Start(ctx)
	if err == nil {
		t.Fatal("expected Start to fail when required capability is rejected")
	}
	if code, ok := CodeOf(err); !ok || code != ErrCapabilityUnavailable {
		t.Fatalf("expected %s, got %s (err: %v)", ErrCapabilityUnavailable, code, err)
	}
}
