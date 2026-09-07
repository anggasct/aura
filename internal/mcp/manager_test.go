package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/toolbroker"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestManagerLifecycleAndToolBrokerIntegration(t *testing.T) {
	ctx := t.Context()

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

	err = mgr.Start(ctx)
	if err == nil {
		t.Fatal("expected Start to fail due to trust required")
	}
	if code, ok := CodeOf(err); !ok || code != ErrTrustRequired {
		t.Fatalf("expected %s, got %s (err: %v)", ErrTrustRequired, code, err)
	}

	namespacedTool := FormatToolName(serverName, "echo")
	for _, def := range broker.Definitions() {
		if def.Name == namespacedTool {
			t.Fatalf("tool %s should not be registered while untrusted", namespacedTool)
		}
	}

	trustRecord, err := trustRegistry.GetTrust(ctx, serverName)
	if err != nil || trustRecord == nil {
		t.Fatalf("expected trust record to exist: %v", err)
	}
	if err := trustRegistry.Approve(ctx, serverName, trustRecord.Digest); err != nil {
		t.Fatalf("Approve failed: %v", err)
	}

	clientTransport2, serverTransport2 := sdk.NewInMemoryTransports()
	go func() {
		_ = server.Run(ctx, serverTransport2)
	}()
	mgr.SetCustomTransport(serverName, clientTransport2)

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

	toolReqNoCaps := *toolReq
	toolReqNoCaps.Capabilities = nil
	_, err = broker.Execute(ctx, &toolReqNoCaps)
	if err == nil {
		t.Fatal("expected execution to fail closed when required capabilities are missing")
	}

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
	if err := trustRegistry.ApproveSpawn(ctx, serverName, rec.SpawnDigest); err != nil {
		t.Fatalf("ApproveSpawn failed: %v", err)
	}

	err = mgr.Start(ctx)
	if err == nil {
		t.Fatal("expected ErrTrustRequired for session digest after discovery")
	}
	if code, _ := CodeOf(err); code != ErrTrustRequired {
		t.Fatalf("expected ErrTrustRequired, got %s", code)
	}
	rec, err = trustRegistry.GetTrust(ctx, serverName)
	if err != nil || rec == nil {
		t.Fatalf("expected trust record with session digest: %v", err)
	}
	if err := trustRegistry.Approve(ctx, serverName, rec.Digest); err != nil {
		t.Fatalf("Approve failed: %v", err)
	}

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

func TestManagerRejectsUnapprovedStdioCommandBeforeSpawn(t *testing.T) {
	ctx := t.Context()

	broker, err := toolbroker.New(&toolbroker.Options{})
	if err != nil {
		t.Fatalf("toolbroker.New failed: %v", err)
	}

	markerPath := filepath.Join(t.TempDir(), "spawned.marker")
	absMarker, err := filepath.Abs(markerPath)
	if err != nil {
		t.Fatal(err)
	}

	scriptPath := filepath.Join(t.TempDir(), "not-allowlisted.sh")
	script := "#!/bin/sh\nprintf x >> \"$1\"\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	serverCfg := config.MCPServer{
		Name:      "unapproved-cmd",
		Transport: config.MCPTransportStdio,
		Command:   scriptPath,
		Args:      []string{absMarker},
	}
	mcpCfg := &config.MCP{Servers: []config.MCPServer{serverCfg}}

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

	err = mgr.Start(ctx)
	if code, ok := CodeOf(err); !ok || code != ErrTrustRequired {
		t.Fatalf("expected %s, got %v", ErrTrustRequired, err)
	}

	if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected no child process spawn (marker must not exist), stat error: %v", err)
	}

	rec, err := trustRegistry.GetTrust(ctx, serverCfg.Name)
	if err != nil || rec == nil {
		t.Fatalf("expected pending trust record for owner review: %v", err)
	}
	if rec.SpawnDecision != TrustDecisionPending || rec.SpawnDigest == "" {
		t.Fatalf("expected pending spawn digest recorded, got decision=%q digest=%q", rec.SpawnDecision, rec.SpawnDigest)
	}
}

func TestManagerDigestChangeForcesSpawnReapproval(t *testing.T) {
	ctx := t.Context()

	broker, err := toolbroker.New(&toolbroker.Options{})
	if err != nil {
		t.Fatalf("toolbroker.New failed: %v", err)
	}

	dir := t.TempDir()
	cmdA := filepath.Join(dir, "server-a.sh")
	cmdB := filepath.Join(dir, "server-b.sh")
	helper := "#!/bin/sh\nexit 0\n"
	for _, path := range []string{cmdA, cmdB} {
		if err := os.WriteFile(path, []byte(helper), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	serverCfg := config.MCPServer{
		Name:           "digest-change",
		Transport:      config.MCPTransportStdio,
		Command:        cmdA,
		StartupTimeout: config.Duration(3 * time.Second),
		RequestTimeout: config.Duration(3 * time.Second),
	}

	trustRegistry := NewMemoryTrustRegistry()
	mgr, err := NewManager(ManagerOptions{
		Config:        &config.MCP{Servers: []config.MCPServer{serverCfg}},
		Broker:        broker,
		TrustRegistry: trustRegistry,
	})
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	if err := mgr.Start(ctx); err == nil {
		t.Fatal("expected first start to require spawn approval")
	}
	rec, err := trustRegistry.GetTrust(ctx, serverCfg.Name)
	if err != nil || rec == nil {
		t.Fatalf("expected trust record after first start: %v", err)
	}
	digestA := rec.SpawnDigest
	if digestA == "" {
		t.Fatal("expected spawn digest recorded after first start")
	}
	if err := trustRegistry.ApproveSpawn(ctx, serverCfg.Name, digestA); err != nil {
		t.Fatalf("ApproveSpawn failed: %v", err)
	}

	err = mgr.Start(ctx)
	if code, _ := CodeOf(err); code != ErrServerUnavailable {
		t.Fatalf("expected past the spawn gate (server unavailable for dead helper), got %v", err)
	}

	mutated := serverCfg
	mutated.Command = cmdB
	mgr2, err := NewManager(ManagerOptions{
		Config:        &config.MCP{Servers: []config.MCPServer{mutated}},
		Broker:        broker,
		TrustRegistry: trustRegistry,
	})
	if err != nil {
		t.Fatalf("NewManager (mutated) failed: %v", err)
	}
	defer func() { _ = mgr2.Close() }()

	err = mgr2.Start(ctx)
	if code, ok := CodeOf(err); !ok || code != ErrTrustRequired {
		t.Fatalf("expected %s after command change, got %v", ErrTrustRequired, err)
	}
	rec, err = trustRegistry.GetTrust(ctx, serverCfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if rec.SpawnDigest == digestA {
		t.Fatal("expected new pending spawn digest after command change")
	}
	if rec.SpawnDecision != TrustDecisionPending {
		t.Fatalf("expected spawn decision reset to pending, got %q", rec.SpawnDecision)
	}

	trusted, err := trustRegistry.IsSpawnTrusted(ctx, serverCfg.Name, rec.SpawnDigest)
	if err != nil {
		t.Fatal(err)
	}
	if trusted {
		t.Fatal("expected new digest to be untrusted until owner re-approval")
	}
	if err := trustRegistry.ApproveSpawn(ctx, serverCfg.Name, rec.SpawnDigest); err != nil {
		t.Fatalf("ApproveSpawn (new digest) failed: %v", err)
	}
	trusted, err = trustRegistry.IsSpawnTrusted(ctx, serverCfg.Name, rec.SpawnDigest)
	if err != nil {
		t.Fatal(err)
	}
	if !trusted {
		t.Fatal("expected new digest trusted after re-approval")
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

func TestManagerCloseDoesNotBlockOnSlowServer(t *testing.T) {
	ctx := t.Context()
	server := sdk.NewServer(&sdk.Implementation{Name: "slow-server", Version: "1.0.0"}, nil)
	server.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			if method == "server/discover" || method == "initialize" {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(5 * time.Second):
					return nil, context.DeadlineExceeded
				}
			}
			return next(ctx, method, req)
		}
	})
	sdk.AddTool(server, &sdk.Tool{Name: "echo", Description: "echo"}, func(_ context.Context, _ *sdk.CallToolRequest, in EchoInput) (*sdk.CallToolResult, EchoOutput, error) {
		return nil, EchoOutput{Reply: in.Message}, nil
	})
	clientTransport, serverTransport := sdk.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, serverTransport) }()

	broker, err := toolbroker.New(&toolbroker.Options{})
	if err != nil {
		t.Fatal(err)
	}
	serverName := "slow-server"
	mcpCfg := &config.MCP{
		Servers: []config.MCPServer{{
			Name:           serverName,
			Transport:      config.MCPTransportStdio,
			RequestTimeout: config.Duration(5 * time.Second),
			StartupTimeout: config.Duration(5 * time.Second),
		}},
	}
	mgr, err := NewManager(ManagerOptions{Config: mcpCfg, Broker: broker, TrustRegistry: NewMemoryTrustRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	mgr.SetCustomTransport(serverName, clientTransport)

	startDone := make(chan error, 1)
	go func() { startDone <- mgr.Start(ctx) }()

	time.Sleep(200 * time.Millisecond)
	closeStart := time.Now()
	if err := mgr.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if elapsed := time.Since(closeStart); elapsed > 2*time.Second {
		t.Fatalf("Close blocked behind slow server: %v", elapsed)
	}
	select {
	case <-startDone:
	case <-time.After(7 * time.Second):
		t.Fatal("Start did not return after slow server settled")
	}
}
