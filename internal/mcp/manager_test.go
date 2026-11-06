package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/capability/capabilitytest"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/effect"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
	"github.com/anggasct/aura/internal/toolbroker"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func newEffectBroker(t *testing.T) *toolbroker.Broker {
	t.Helper()
	ctx := t.Context()
	db := storetest.OpenMigrated(t)
	session := store.Session{ID: "session-1", OwnerID: "principal-1"}
	if err := store.NewSessionService(db).Create(ctx, &session); err != nil {
		t.Fatalf("Create(): %v", err)
	}
	journal, err := effect.NewJournal(db, effect.Options{})
	if err != nil {
		t.Fatalf("NewJournal(): %v", err)
	}
	effects, err := effect.NewExecutor(journal)
	if err != nil {
		t.Fatalf("NewExecutor(): %v", err)
	}
	broker, err := toolbroker.New(&toolbroker.Options{
		HostCapabilities: capabilitytest.Host(t), Effects: effects,
		ApprovalDecider: func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) {
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("toolbroker.New(): %v", err)
	}
	return broker
}

func withEffectIdentity(request *toolbroker.ToolRequest, key string) *toolbroker.ToolRequest {
	request.IdempotencyKey = key
	request.EventSequence = 1
	request.EventInvocation = "inv-" + key
	request.EventBranch = "main"
	request.EventAuthor = "agent"
	return request
}

func approveConfigDigest(ctx context.Context, t *testing.T, registry TrustRegistry, serverCfg *config.MCPServer) {
	t.Helper()
	digest, err := ComputeConfigDigest(serverCfg)
	if err != nil {
		t.Fatalf("ComputeConfigDigest(): %v", err)
	}
	if err := registry.ApproveConfig(ctx, serverCfg.Name, digest); err != nil {
		t.Fatalf("ApproveConfig(): %v", err)
	}
}

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

	broker := newEffectBroker(t)

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

	mgr, err := NewManager(&ManagerOptions{
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

	approveConfigDigest(t.Context(), t, trustRegistry, &mcpCfg.Servers[0])

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
	toolReq := withEffectIdentity(&toolbroker.ToolRequest{
		RequestID:    "req-1",
		TurnID:       "turn-1",
		SessionID:    "session-1",
		PrincipalID:  "principal-1",
		ToolName:     namespacedTool,
		ToolVersion:  "v1",
		Arguments:    reqArgs,
		Capabilities: []string{"workspace-read"},
		Trust:        approval.TrustOwnerInput,
	}, "lifecycle-1")

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

	if err := mgr.Close(ctx); err != nil {
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

	broker := newEffectBroker(t)

	serverName := "stdio-e2e"
	serverCfg := config.MCPServer{
		Name:           serverName,
		Transport:      config.MCPTransportStdio,
		Command:        "/bin/stdio-e2e",
		Environment:    map[string]string{"SERVER_MODE": "test"},
		StartupTimeout: config.Duration(10 * time.Second),
		RequestTimeout: config.Duration(10 * time.Second),
		MaxMessageSize: 1024 * 1024,
	}

	mcpCfg := &config.MCP{
		Servers: []config.MCPServer{serverCfg},
	}

	trustRegistry := NewMemoryTrustRegistry()
	starter := &stubSessionStarter{session: &scriptedSession{toolNames: []string{"ping"}}}

	mgr, err := NewManager(&ManagerOptions{
		Config:         mcpCfg,
		Broker:         broker,
		TrustRegistry:  trustRegistry,
		SessionStarter: starter,
	})
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer func() { _ = mgr.Close(t.Context()) }()

	err = mgr.Start(ctx)
	if err == nil {
		t.Fatal("expected ErrTrustRequired on first start without approval")
	}
	if code, _ := CodeOf(err); code != ErrTrustRequired {
		t.Fatalf("expected ErrTrustRequired, got %s", code)
	}
	if starter.starts() != 0 {
		t.Fatalf("unapproved server started %d sessions", starter.starts())
	}

	approveConfigDigest(t.Context(), t, trustRegistry, &serverCfg)
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	namespacedTool := FormatToolName(serverName, "ping")
	callArgs, err := json.Marshal(EchoInput{Message: "hello"})
	if err != nil {
		t.Fatalf("marshal callArgs failed: %v", err)
	}
	toolReq := withEffectIdentity(&toolbroker.ToolRequest{
		RequestID:   "req-2",
		TurnID:      "turn-1",
		SessionID:   "session-1",
		PrincipalID: "principal-1",
		ToolName:    namespacedTool,
		ToolVersion: "v1",
		Arguments:   callArgs,
		Trust:       approval.TrustOwnerInput,
	}, "stdio-e2e-1")

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

	broker, err := toolbroker.New(&toolbroker.Options{HostCapabilities: capabilitytest.Host(t)})
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
	mgr, err := NewManager(&ManagerOptions{
		Config:        mcpCfg,
		Broker:        broker,
		TrustRegistry: trustRegistry,
	})
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer func() { _ = mgr.Close(t.Context()) }()

	err = mgr.Start(ctx)
	if code, ok := CodeOf(err); !ok || code != ErrTrustRequired {
		t.Fatalf("expected %s, got %v", ErrTrustRequired, err)
	}

	if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected no child process spawn (marker must not exist), stat error: %v", err)
	}

	if _, err := trustRegistry.GetTrust(ctx, serverCfg.Name); !errors.Is(err, ErrTrustNotFound) {
		t.Fatalf("an unapproved start must not write trust state, got %v", err)
	}
}

func TestManagerDigestChangeForcesReapproval(t *testing.T) {
	ctx := t.Context()

	broker := newEffectBroker(t)

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
	starter := &stubSessionStarter{session: &failingSession{err: errors.New("child exited")}}
	mgr, err := NewManager(&ManagerOptions{
		Config:         &config.MCP{Servers: []config.MCPServer{serverCfg}},
		Broker:         broker,
		TrustRegistry:  trustRegistry,
		SessionStarter: starter,
	})
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer func() { _ = mgr.Close(t.Context()) }()

	if err := mgr.Start(ctx); err == nil {
		t.Fatal("expected first start to require approval")
	}
	approveConfigDigest(t.Context(), t, trustRegistry, &serverCfg)

	err = mgr.Start(ctx)
	if code, _ := CodeOf(err); code != ErrServerUnavailable {
		t.Fatalf("expected past the trust gate (server unavailable for dead helper), got %v", err)
	}
	startsBeforeChange := starter.starts()
	if startsBeforeChange == 0 {
		t.Fatal("approved server was never started")
	}

	mutated := serverCfg
	mutated.Command = cmdB
	mgr2, err := NewManager(&ManagerOptions{
		Config:         &config.MCP{Servers: []config.MCPServer{mutated}},
		Broker:         broker,
		TrustRegistry:  trustRegistry,
		SessionStarter: starter,
	})
	if err != nil {
		t.Fatalf("NewManager (mutated) failed: %v", err)
	}
	defer func() { _ = mgr2.Close(t.Context()) }()

	err = mgr2.Start(ctx)
	if code, ok := CodeOf(err); !ok || code != ErrTrustRequired {
		t.Fatalf("expected %s after command change, got %v", ErrTrustRequired, err)
	}
	if starter.starts() != startsBeforeChange {
		t.Fatal("a changed command was started before re-approval")
	}
	approveConfigDigest(t.Context(), t, trustRegistry, &mutated)
	err = mgr2.Start(ctx)
	if code, _ := CodeOf(err); code != ErrServerUnavailable {
		t.Fatalf("expected past the trust gate after re-approval, got %v", err)
	}
}

func TestManagerCapabilityCheckFailure(t *testing.T) {
	ctx := t.Context()
	broker, err := toolbroker.New(&toolbroker.Options{HostCapabilities: capabilitytest.Host(t)})
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

	mgr, err := NewManager(&ManagerOptions{
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

	broker := newEffectBroker(t)
	serverName := "slow-server"
	mcpCfg := &config.MCP{
		Servers: []config.MCPServer{{
			Name:           serverName,
			Transport:      config.MCPTransportStdio,
			RequestTimeout: config.Duration(5 * time.Second),
			StartupTimeout: config.Duration(5 * time.Second),
		}},
	}
	registry := NewMemoryTrustRegistry()
	approveConfigDigest(t.Context(), t, registry, &mcpCfg.Servers[0])
	mgr, err := NewManager(&ManagerOptions{Config: mcpCfg, Broker: broker, TrustRegistry: registry})
	if err != nil {
		t.Fatal(err)
	}
	mgr.SetCustomTransport(serverName, clientTransport)

	startDone := make(chan error, 1)
	go func() { startDone <- mgr.Start(ctx) }()

	time.Sleep(200 * time.Millisecond)
	closeStart := time.Now()
	if err := mgr.Close(ctx); err != nil {
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

func TestManagerFailedConnectClosesClient(t *testing.T) {
	ctx := t.Context()
	serverCfg := config.MCPServer{
		Name:           "leaky-connect",
		Transport:      config.MCPTransportStdio,
		Command:        "/bin/leaky-server",
		StartupTimeout: config.Duration(10 * time.Second),
		RequestTimeout: config.Duration(10 * time.Second),
		MaxMessageSize: 1024 * 1024,
	}
	broker, err := toolbroker.New(&toolbroker.Options{HostCapabilities: capabilitytest.Host(t)})
	if err != nil {
		t.Fatal(err)
	}
	failing := &failingSession{err: errors.New("child failed during handshake")}
	starter := &stubSessionStarter{session: failing}
	trustRegistry := NewMemoryTrustRegistry()
	mgr, err := NewManager(&ManagerOptions{
		Config:         &config.MCP{Servers: []config.MCPServer{serverCfg}},
		Broker:         broker,
		TrustRegistry:  trustRegistry,
		SessionStarter: starter,
	})
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer func() { _ = mgr.Close(t.Context()) }()

	approveConfigDigest(t.Context(), t, trustRegistry, &serverCfg)
	err = mgr.Start(ctx)
	if err == nil {
		t.Fatal("expected Start to fail when the session breaks during connect")
	}
	if code, ok := CodeOf(err); !ok || code != ErrServerUnavailable {
		t.Fatalf("expected %s, got %s (%v)", ErrServerUnavailable, code, err)
	}
	starter.mu.Lock()
	defer starter.mu.Unlock()
	if failing.closed != 1 {
		t.Error("failed connect leaked the contained session")
	}
}

func startableStdioConfig(name string) config.MCPServer {
	return config.MCPServer{
		Name:           name,
		Transport:      config.MCPTransportStdio,
		Command:        "/bin/" + name,
		StartupTimeout: config.Duration(5 * time.Second),
		RequestTimeout: config.Duration(5 * time.Second),
		MaxMessageSize: 1 << 20,
	}
}

func brokerHasTool(broker *toolbroker.Broker, name string) bool {
	listed := broker.Definitions()
	for i := range listed {
		def := &listed[i]
		if def.Name == name {
			return true
		}
	}
	return false
}

func TestManagerFailingServerDoesNotStopOthers(t *testing.T) {
	ctx := t.Context()
	broker := newEffectBroker(t)
	healthy := startableStdioConfig("healthy")
	broken := startableStdioConfig("broken")
	untrusted := startableStdioConfig("untrusted")
	registry := NewMemoryTrustRegistry()
	approveConfigDigest(t.Context(), t, registry, &healthy)
	approveConfigDigest(t.Context(), t, registry, &broken)
	starter := &perServerStarter{sessions: map[string]ContainedSession{
		"healthy": &scriptedSession{toolNames: []string{"ping"}},
		"broken":  &failingSession{err: errors.New("child failed")},
	}}
	mgr, err := NewManager(&ManagerOptions{
		Config:         &config.MCP{Servers: []config.MCPServer{broken, untrusted, healthy}},
		Broker:         broker,
		TrustRegistry:  registry,
		SessionStarter: starter,
	})
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	defer func() { _ = mgr.Close(t.Context()) }()

	err = mgr.Start(ctx)
	if err == nil {
		t.Fatal("Start() = nil, want the joined per-server failures")
	}
	if !brokerHasTool(broker, FormatToolName("healthy", "ping")) {
		t.Error("a failing sibling prevented the healthy server from registering")
	}
	if starter.started("untrusted") != 0 {
		t.Error("an unapproved server was started")
	}
	want := map[string]ErrorCode{"healthy": "", "broken": ErrServerUnavailable, "untrusted": ErrTrustRequired}
	for _, status := range mgr.Statuses() {
		if status.Code != want[status.Name] {
			t.Errorf("status[%s] = %q, want %q", status.Name, status.Code, want[status.Name])
		}
	}
}

type perServerStarter struct {
	mu       sync.Mutex
	sessions map[string]ContainedSession
	counts   map[string]int
}

func (s *perServerStarter) StartSession(_ context.Context, req *ContainedSessionRequest) (ContainedSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.counts == nil {
		s.counts = map[string]int{}
	}
	s.counts[req.ServerName]++
	session, ok := s.sessions[req.ServerName]
	if !ok {
		return nil, errors.New("no session scripted")
	}
	return session, nil
}

func (s *perServerStarter) started(server string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[server]
}

func TestManagerPinsToolSurfaceOnFirstStartAndRequiresReapprovalOnDrift(t *testing.T) {
	ctx := t.Context()
	broker := newEffectBroker(t)
	cfg := startableStdioConfig("drift")
	registry := NewMemoryTrustRegistry()
	approveConfigDigest(t.Context(), t, registry, &cfg)

	first, err := NewManager(&ManagerOptions{
		Config:         &config.MCP{Servers: []config.MCPServer{cfg}},
		Broker:         broker,
		TrustRegistry:  registry,
		SessionStarter: &stubSessionStarter{session: &scriptedSession{toolNames: []string{"a"}}},
	})
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	if err := first.Start(ctx); err != nil {
		t.Fatalf("first Start(): %v", err)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	review, err := ReviewServer(ctx, registry, &cfg)
	if err != nil || review.State != TrustStateApproved {
		t.Fatalf("review after pin = %+v, %v", review, err)
	}

	second, err := NewManager(&ManagerOptions{
		Config:         &config.MCP{Servers: []config.MCPServer{cfg}},
		Broker:         broker,
		TrustRegistry:  registry,
		SessionStarter: &stubSessionStarter{session: &scriptedSession{toolNames: []string{"a", "b"}}},
	})
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	defer func() { _ = second.Close(t.Context()) }()
	err = second.Start(ctx)
	if code, ok := CodeOf(err); !ok || code != ErrTrustRequired {
		t.Fatalf("drifted tool surface started: %v", err)
	}
	if brokerHasTool(broker, FormatToolName("drift", "a")) || brokerHasTool(broker, FormatToolName("drift", "b")) {
		t.Error("tools of a drifted server are registered")
	}
	review, err = ReviewServer(ctx, registry, &cfg)
	if err != nil || review.State != TrustStateDigestChanged {
		t.Fatalf("review after drift = %+v, %v", review, err)
	}
	result, err := ApproveServer(ctx, registry, &cfg)
	if err != nil || result.Change != ApprovalChangeToolSurface || !slices.Equal(result.Tools, []string{"a", "b"}) {
		t.Fatalf("ApproveServer() = %+v, %v", result, err)
	}
	if err := second.Start(ctx); err != nil {
		t.Fatalf("Start() after tool surface approval: %v", err)
	}
	if !brokerHasTool(broker, FormatToolName("drift", "b")) {
		t.Error("approved tool surface was not registered")
	}
}

func TestManagerRegistersToolsAsEffectfulWithApproval(t *testing.T) {
	ctx := t.Context()
	broker := newEffectBroker(t)
	cfg := startableStdioConfig("fx")
	cfg.Capabilities = []string{"fs.read"}
	registry := NewMemoryTrustRegistry()
	approveConfigDigest(t.Context(), t, registry, &cfg)
	mgr, err := NewManager(&ManagerOptions{
		Config:         &config.MCP{Servers: []config.MCPServer{cfg}},
		Broker:         broker,
		TrustRegistry:  registry,
		SessionStarter: &stubSessionStarter{session: &scriptedSession{toolNames: []string{"ping"}}},
	})
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	defer func() { _ = mgr.Close(t.Context()) }()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start(): %v", err)
	}
	var found bool
	for _, def := range broker.Definitions() {
		if def.Name != FormatToolName("fx", "ping") {
			continue
		}
		found = true
		if !def.Effectful || !def.RequiresApproval || def.Server != "fx" {
			t.Errorf("definition = effectful %v approval %v server %q", def.Effectful, def.RequiresApproval, def.Server)
		}
		if len(def.Schema) == 0 {
			t.Error("definition has no schema")
		}
	}
	if !found {
		t.Fatal("tool not registered")
	}
	request := withEffectIdentity(&toolbroker.ToolRequest{
		RequestID: "r1", TurnID: "t1", SessionID: "session-1", PrincipalID: "principal-1",
		ToolName: FormatToolName("fx", "ping"), ToolVersion: "v1", Arguments: json.RawMessage(`{}`),
		Capabilities: []string{"fs.read"}, Trust: approval.TrustOwnerInput,
	}, "fx-1")
	request.PrincipalClass = "trusted"
	if _, err := broker.Execute(ctx, request); err == nil {
		t.Fatal("a non-owner principal executed an mcp tool")
	}
}

func TestManagerSkipsToolNamesProvidedByAnotherServer(t *testing.T) {
	ctx := t.Context()
	broker := newEffectBroker(t)
	first := startableStdioConfig("a")
	second := startableStdioConfig("a_b")
	registry := NewMemoryTrustRegistry()
	approveConfigDigest(t.Context(), t, registry, &first)
	approveConfigDigest(t.Context(), t, registry, &second)
	starter := &perServerStarter{sessions: map[string]ContainedSession{
		"a":   &scriptedSession{toolNames: []string{"b_c", "solo"}},
		"a_b": &scriptedSession{toolNames: []string{"c", "other"}},
	}}
	mgr, err := NewManager(&ManagerOptions{
		Config:         &config.MCP{Servers: []config.MCPServer{first, second}},
		Broker:         broker,
		TrustRegistry:  registry,
		SessionStarter: starter,
	})
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	defer func() { _ = mgr.Close(t.Context()) }()
	_ = mgr.Start(ctx)
	for _, status := range mgr.Statuses() {
		if status.Code != "" {
			t.Fatalf("server %q status = %q, want both servers running", status.Name, status.Code)
		}
	}
	counts := map[string]int{}
	for _, def := range broker.Definitions() {
		counts[def.Name]++
	}
	if counts["mcp_a_b_c"] != 1 {
		t.Fatalf("colliding tool registered %d times, want once", counts["mcp_a_b_c"])
	}
	for _, name := range []string{"mcp_a_solo", "mcp_a_b_other"} {
		if counts[name] != 1 {
			t.Errorf("tool %q registered %d times, want the non-colliding tools of both servers served", name, counts[name])
		}
	}
}

func TestManagerSkipsToolsWhoseExposedNameIsTooLong(t *testing.T) {
	ctx := t.Context()
	broker := newEffectBroker(t)
	cfg := startableStdioConfig("long")
	registry := NewMemoryTrustRegistry()
	approveConfigDigest(t.Context(), t, registry, &cfg)
	long := strings.Repeat("x", 60)
	mgr, err := NewManager(&ManagerOptions{
		Config:         &config.MCP{Servers: []config.MCPServer{cfg}},
		Broker:         broker,
		TrustRegistry:  registry,
		SessionStarter: &stubSessionStarter{session: &scriptedSession{toolNames: []string{"ok", long}}},
	})
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	defer func() { _ = mgr.Close(t.Context()) }()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start(): %v", err)
	}
	if !brokerHasTool(broker, FormatToolName("long", "ok")) {
		t.Error("short tool missing")
	}
	if brokerHasTool(broker, FormatToolName("long", long)) {
		t.Error("tool with an over-long exposed name registered")
	}
}

func TestManagerUnexposedEntries(t *testing.T) {
	ctx := t.Context()
	broker := newEffectBroker(t)
	up := startableStdioConfig("up")
	down := startableStdioConfig("down")
	registry := NewMemoryTrustRegistry()
	approveConfigDigest(t.Context(), t, registry, &up)
	mgr, err := NewManager(&ManagerOptions{
		Config:         &config.MCP{Servers: []config.MCPServer{up, down}},
		Broker:         broker,
		TrustRegistry:  registry,
		SessionStarter: &perServerStarter{sessions: map[string]ContainedSession{"up": &scriptedSession{toolNames: []string{"ping"}}}},
	})
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	defer func() { _ = mgr.Close(t.Context()) }()
	_ = mgr.Start(ctx)
	got := mgr.UnexposedEntries([]string{"read_file", "mcp_up", "mcp_up_ping", "mcp_up_missing", "mcp_down_anything", "mcp_down"})
	if !slices.Equal(got, []string{"mcp_up_missing"}) {
		t.Fatalf("UnexposedEntries() = %v, want only the missing tool of the running server", got)
	}
}

func TestManagerCloseLeavesNoSessionsOpen(t *testing.T) {
	ctx := t.Context()
	broker := newEffectBroker(t)
	cfg := startableStdioConfig("clean")
	registry := NewMemoryTrustRegistry()
	approveConfigDigest(t.Context(), t, registry, &cfg)
	session := &scriptedSession{toolNames: []string{"ping"}}
	mgr, err := NewManager(&ManagerOptions{
		Config:         &config.MCP{Servers: []config.MCPServer{cfg}},
		Broker:         broker,
		TrustRegistry:  registry,
		SessionStarter: &stubSessionStarter{session: session},
	})
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start(): %v", err)
	}
	if err := mgr.Close(ctx); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	session.mu.Lock()
	closed := session.closed
	session.mu.Unlock()
	if closed != 1 {
		t.Errorf("contained session closed %d times, want 1", closed)
	}
	if brokerHasTool(broker, FormatToolName("clean", "ping")) {
		t.Error("tool still registered after Close")
	}
	if err := mgr.Start(ctx); err == nil {
		t.Error("closed manager started again")
	}
}

type unresponsiveSession struct {
	mu     sync.Mutex
	closed bool
}

func (s *unresponsiveSession) Request(ctx context.Context, _ []byte) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(600 * time.Millisecond):
		return nil, errors.New("exchange deadline exceeded")
	}
}

func (s *unresponsiveSession) Close(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func TestManagerStartIsBoundedByStartupTimeout(t *testing.T) {
	ctx := t.Context()
	broker := newEffectBroker(t)
	slow := startableStdioConfig("slow")
	slow.StartupTimeout = config.Duration(300 * time.Millisecond)
	fast := startableStdioConfig("fast")
	registry := NewMemoryTrustRegistry()
	approveConfigDigest(t.Context(), t, registry, &slow)
	approveConfigDigest(t.Context(), t, registry, &fast)
	stuck := &unresponsiveSession{}
	mgr, err := NewManager(&ManagerOptions{
		Config:        &config.MCP{Servers: []config.MCPServer{slow, fast}},
		Broker:        broker,
		TrustRegistry: registry,
		SessionStarter: &perServerStarter{sessions: map[string]ContainedSession{
			"slow": stuck,
			"fast": &scriptedSession{toolNames: []string{"ping"}},
		}},
	})
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	defer func() { _ = mgr.Close(t.Context()) }()

	started := time.Now()
	err = mgr.Start(ctx)
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("Start blocked for %v behind an unresponsive server", elapsed)
	}
	if err == nil {
		t.Fatal("Start() = nil, want the slow server's failure")
	}
	if !brokerHasTool(broker, FormatToolName("fast", "ping")) {
		t.Error("the responsive server was not registered")
	}
	for _, status := range mgr.Statuses() {
		if status.Name == "slow" && status.Code == "" {
			t.Error("unresponsive server reported as running")
		}
	}
	stuck.mu.Lock()
	defer stuck.mu.Unlock()
	if !stuck.closed {
		t.Error("unresponsive session was not closed")
	}
}
