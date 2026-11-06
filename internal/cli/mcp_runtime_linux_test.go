package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/capability"
	"github.com/anggasct/aura/internal/capability/capabilitytest"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/mcp"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/runtime/adk"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
	"github.com/anggasct/aura/internal/toolbroker"
	"github.com/anggasct/aura/internal/tools/builtin"
)

type fixtureSession struct {
	mu      sync.Mutex
	pending [][]byte
	tools   []string
	calls   int
	closed  bool
	broken  bool
}

func (s *fixtureSession) Request(_ context.Context, payload []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken {
		return nil, errors.New("child exited")
	}
	for _, line := range strings.Split(string(payload), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var header struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal([]byte(line), &header); err != nil {
			return nil, errors.New("malformed frame")
		}
		id := fixtureID(header.ID)
		switch header.Method {
		case "ping":
			s.pending = append(s.pending, []byte(`{"jsonrpc":"2.0","id":`+id+`,"result":{}}`))
		case "initialize":
			s.pending = append(s.pending, []byte(`{"jsonrpc":"2.0","id":`+id+`,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"fixture","version":"1"}}}`))
		case "tools/list":
			entries := make([]string, 0, len(s.tools))
			for _, name := range s.tools {
				entries = append(entries, `{"name":"`+name+`","description":"`+name+` tool","inputSchema":{"type":"object"}}`)
			}
			s.pending = append(s.pending, []byte(`{"jsonrpc":"2.0","id":`+id+`,"result":{"tools":[`+strings.Join(entries, ",")+`]}}`))
		case "tools/call":
			s.calls++
			s.pending = append(s.pending, []byte(`{"jsonrpc":"2.0","id":`+id+`,"result":{"content":[{"type":"text","text":"pong"}]}}`))
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

func fixtureID(id any) string {
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

func (s *fixtureSession) Close(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *fixtureSession) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *fixtureSession) breakChild() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.broken = true
}

type fixtureStarter struct {
	mu       sync.Mutex
	tools    map[string][]string
	failing  map[string]bool
	requests []mcp.ContainedSessionRequest
	sessions map[string]*fixtureSession
}

func newFixtureStarter(tools map[string][]string) *fixtureStarter {
	return &fixtureStarter{tools: tools, failing: map[string]bool{}, sessions: map[string]*fixtureSession{}}
}

func (s *fixtureStarter) StartSession(_ context.Context, req *mcp.ContainedSessionRequest) (mcp.ContainedSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, *req)
	if s.failing[req.ServerName] {
		return nil, errors.New("sandbox refused")
	}
	session := &fixtureSession{tools: s.tools[req.ServerName]}
	s.sessions[req.ServerName] = session
	return session, nil
}

func (s *fixtureStarter) started(server string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for i := range s.requests {
		if s.requests[i].ServerName == server {
			count++
		}
	}
	return count
}

func (s *fixtureStarter) session(server string) *fixtureSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[server]
}

func (s *fixtureStarter) openSessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	open := 0
	for _, session := range s.sessions {
		session.mu.Lock()
		if !session.closed {
			open++
		}
		session.mu.Unlock()
	}
	return open
}

type mcpHarness struct {
	cfg      config.Config
	db       *sql.DB
	executor *toolsbuiltin.Executor
	report   capability.Report
	prompts  []string
	shown    []string
	approve  bool
	sequence uint64
	mu       sync.Mutex
}

func newMCPHarness(t *testing.T, servers []config.MCPServer) *mcpHarness {
	t.Helper()
	return newMCPHarnessWithAccess(t, servers, nil)
}

func newMCPHarnessWithAccess(t *testing.T, servers []config.MCPServer, access func(db *sql.DB) toolbroker.AccessPolicy) *mcpHarness {
	t.Helper()
	ctx := t.Context()
	dataRoot := t.TempDir()
	storetest.Seed(t, filepath.Join(dataRoot, "aura.db"))
	db, err := store.OpenDB(ctx, filepath.Join(dataRoot, "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	session := store.Session{ID: "session-1", OwnerID: "owner-1"}
	if err := store.NewSessionService(db).Create(ctx, &session); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	harness := &mcpHarness{cfg: config.Default(), db: db, report: capabilitytest.All(t), approve: true}
	harness.cfg.Tools.Workspace = t.TempDir()
	harness.cfg.Storage.Path = dataRoot
	harness.cfg.MCP = &config.MCP{Servers: servers}
	var policy toolbroker.AccessPolicy
	if access != nil {
		policy = access(db)
	}
	executor, err := toolsbuiltin.New(&harness.cfg, db, dataRoot, harness.report, slog.New(slog.DiscardHandler), nil, func(_ context.Context, prompt *toolbroker.ApprovalPrompt) (bool, error) {
		harness.mu.Lock()
		defer harness.mu.Unlock()
		harness.prompts = append(harness.prompts, prompt.ToolName)
		harness.shown = append(harness.shown, prompt.Arguments)
		return harness.approve, nil
	}, policy)
	if err != nil {
		t.Fatalf("toolsbuiltin.New: %v", err)
	}
	harness.executor = executor
	return harness
}

func (h *mcpHarness) start(t *testing.T, starter mcp.SessionStarter) *mcpRuntime {
	t.Helper()
	trustPath, err := mcpTrustRegistryPath(&h.cfg)
	if err != nil {
		t.Fatalf("mcpTrustRegistryPath: %v", err)
	}
	started, err := startMCPRuntime(t.Context(), &mcpRuntimeOptions{
		Config:         &h.cfg,
		Broker:         h.executor.Broker(),
		Logger:         slog.New(slog.DiscardHandler),
		Capabilities:   h.report,
		SessionStarter: starter,
		TrustPath:      trustPath,
	})
	if err != nil {
		t.Fatalf("startMCPRuntime: %v", err)
	}
	if started == nil {
		t.Fatal("no runtime for a configured server")
	}
	t.Cleanup(func() { _ = started.Close(context.WithoutCancel(t.Context())) })
	return started
}

func (h *mcpHarness) approveServer(t *testing.T, name string) {
	t.Helper()
	trustPath, err := mcpTrustRegistryPath(&h.cfg)
	if err != nil {
		t.Fatalf("mcpTrustRegistryPath: %v", err)
	}
	registry, err := mcp.NewFileTrustRegistry(trustPath)
	if err != nil {
		t.Fatalf("NewFileTrustRegistry: %v", err)
	}
	server, ok := findMCPServer(&h.cfg, name)
	if !ok {
		t.Fatalf("server %s not configured", name)
	}
	if _, err := mcp.ApproveServer(t.Context(), registry, server); err != nil {
		t.Fatalf("ApproveServer: %v", err)
	}
}

func (h *mcpHarness) toolNames() []string {
	var names []string
	definitions := h.executor.Definitions()
	for i := range definitions {
		names = append(names, definitions[i].Name)
	}
	return names
}

func (h *mcpHarness) promptedArguments() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.shown)
}

func (h *mcpHarness) promptedTools() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.prompts)
}

func (h *mcpHarness) callTool(ctx context.Context, name, id string, class principal.Class) error {
	return h.callToolAs(ctx, name, id, class, "")
}

func (h *mcpHarness) callToolAs(ctx context.Context, name, id string, class principal.Class, agentID string) error {
	h.mu.Lock()
	h.sequence++
	sequence := h.sequence
	h.mu.Unlock()
	_, err := h.executor.Execute(ctx, &runtimeadk.BuiltinToolRequest{
		RequestID: id, TurnID: "turn-" + id, SessionID: "session-1", PrincipalID: "owner-1", PrincipalClass: class, AgentID: agentID,
		ToolName: name, ToolVersion: "v1", Arguments: json.RawMessage(`{}`),
		Capabilities: []string{"public-web"}, Trust: string(approval.TrustDerivedUntrusted),
		IdempotencyKey: "key-" + id, EventSequence: sequence, EventInvocation: "inv-" + id, EventBranch: "main", EventAuthor: "agent",
	})
	return err
}

func mcpTestServer(name string) config.MCPServer {
	return config.MCPServer{
		Name:           name,
		Transport:      config.MCPTransportStdio,
		Command:        "/opt/mcp/" + name,
		Args:           []string{"--stdio"},
		Environment:    map[string]string{"MODE": "test"},
		Capabilities:   []string{"public-web"},
		StartupTimeout: config.Duration(5 * time.Second),
		RequestTimeout: config.Duration(5 * time.Second),
		MaxMessageSize: 1 << 20,
	}
}

func findingCodes(findings []health.Finding) map[string]string {
	codes := make(map[string]string, len(findings))
	for i := range findings {
		codes[findings[i].Scope] = findings[i].Code
	}
	return codes
}

func countIntents(t *testing.T, db *sql.DB) int {
	t.Helper()
	var intents int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM effect_intent`).Scan(&intents); err != nil {
		t.Fatalf("count effect intents: %v", err)
	}
	return intents
}

func TestMCPRuntimeStartsApprovedServerAndRoutesCallsThroughTheBroker(t *testing.T) {
	harness := newMCPHarness(t, []config.MCPServer{mcpTestServer("docs")})
	harness.approveServer(t, "docs")
	starter := newFixtureStarter(map[string][]string{"docs": {"ping"}})
	started := harness.start(t, starter)

	if starter.started("docs") != 1 {
		t.Fatalf("sessions started = %d, want 1", starter.started("docs"))
	}
	request := starter.requests[0]
	if request.Executable != "/opt/mcp/docs" || !slices.Equal(request.Arguments, []string{"--stdio"}) || request.Environment["MODE"] != "test" {
		t.Errorf("session request = %+v", request)
	}
	if !slices.Contains(harness.toolNames(), "mcp_docs_ping") {
		t.Fatalf("broker tools = %v, want mcp_docs_ping", harness.toolNames())
	}
	for _, definition := range harness.executor.Definitions() {
		if definition.Name == "mcp_docs_ping" && definition.Server != "docs" {
			t.Errorf("definition server = %q", definition.Server)
		}
	}

	if err := harness.callTool(t.Context(), "mcp_docs_ping", "c1", principal.ClassOwner); err != nil {
		t.Fatalf("owner call: %v", err)
	}
	if got := harness.promptedTools(); !slices.Equal(got, []string{"mcp_docs_ping"}) {
		t.Errorf("approval prompts = %v, want one for the mcp tool", got)
	}
	if intents := countIntents(t, harness.db); intents != 1 {
		t.Errorf("effect intents = %d, want 1", intents)
	}
	if calls := starter.session("docs").callCount(); calls != 1 {
		t.Errorf("tool calls reaching the server = %d, want 1", calls)
	}

	harness.mu.Lock()
	harness.approve = false
	harness.mu.Unlock()
	if err := harness.callTool(t.Context(), "mcp_docs_ping", "c2", principal.ClassOwner); err == nil {
		t.Fatal("a rejected approval still ran the tool")
	}
	if calls := starter.session("docs").callCount(); calls != 1 {
		t.Errorf("tool calls after a rejected approval = %d, want 1", calls)
	}

	findings := started.check.Checker.Check(t.Context())
	if len(findings) != 1 || findings[0].Code != "ok" {
		t.Errorf("health findings = %+v, want a single ok", findings)
	}
	if started.check.ID != "mcp" || started.check.Remediation != "review-mcp-server" {
		t.Errorf("check = %q remediation %q", started.check.ID, started.check.Remediation)
	}
}

func TestMCPToolsAreDeniedToNonOwnersAtTheBroker(t *testing.T) {
	harness := newMCPHarness(t, []config.MCPServer{mcpTestServer("docs")})
	harness.approveServer(t, "docs")
	starter := newFixtureStarter(map[string][]string{"docs": {"ping"}})
	harness.start(t, starter)
	for _, class := range []principal.Class{principal.ClassTrusted, principal.ClassPublic} {
		if err := harness.callTool(t.Context(), "mcp_docs_ping", "np-"+string(class), class); err == nil {
			t.Errorf("%s principal ran an mcp tool", class)
		}
	}
	if calls := starter.session("docs").callCount(); calls != 0 {
		t.Errorf("non-owner calls reached the server: %d", calls)
	}
	if prompts := harness.promptedTools(); len(prompts) != 0 {
		t.Errorf("non-owner calls raised approval prompts: %v", prompts)
	}
}

func TestTrustedPrincipalsReachMCPToolsOnlyThroughPrincipalToolsAndTheOwner(t *testing.T) {
	registry, err := auraagent.BuildWithMCP([]config.AgentDefinition{
		{ID: "team", Description: "Team", Instructions: "Help.", Audience: config.AudienceTrusted, Tools: []string{"mcp_docs"}, PrincipalTools: []string{"mcp_docs_ping"}},
		{ID: "plain", Description: "Plain", Instructions: "Help.", Audience: config.AudienceTrusted, Tools: []string{"mcp_docs"}},
	}, []string{"read_file"}, []string{"docs"}, []string{"primary"})
	if err != nil {
		t.Fatalf("BuildWithMCP: %v", err)
	}
	harness := newMCPHarnessWithAccess(t, []config.MCPServer{mcpTestServer("docs")}, func(db *sql.DB) toolbroker.AccessPolicy {
		return newAccessPolicy(registry, db, nil)
	})
	harness.approveServer(t, "docs")
	starter := newFixtureStarter(map[string][]string{"docs": {"ping"}})
	harness.start(t, starter)

	if err := harness.callToolAs(t.Context(), "mcp_docs_ping", "t1", principal.ClassTrusted, "team"); err != nil {
		t.Fatalf("trusted call through principal_tools: %v", err)
	}
	if got := harness.promptedTools(); !slices.Equal(got, []string{"mcp_docs_ping"}) {
		t.Errorf("approval prompts = %v, want one owner card for the mcp tool", got)
	}
	if intents := countIntents(t, harness.db); intents != 1 {
		t.Errorf("effect intents = %d, want 1", intents)
	}
	if calls := starter.session("docs").callCount(); calls != 1 {
		t.Errorf("tool calls reaching the server = %d, want 1", calls)
	}

	harness.mu.Lock()
	harness.approve = false
	harness.mu.Unlock()
	if err := harness.callToolAs(t.Context(), "mcp_docs_ping", "t2", principal.ClassTrusted, "team"); err == nil {
		t.Fatal("a rejected owner approval still ran the tool")
	}
	if calls := starter.session("docs").callCount(); calls != 1 {
		t.Errorf("tool calls after a rejected approval = %d, want 1", calls)
	}

	prompts := len(harness.promptedTools())
	denied := map[string]struct {
		class   principal.Class
		agentID string
	}{
		"persona without a principal entry": {principal.ClassTrusted, "plain"},
		"unknown persona":                   {principal.ClassTrusted, "ghost"},
		"public principal":                  {principal.ClassPublic, "team"},
	}
	for name, tc := range denied {
		if err := harness.callToolAs(t.Context(), "mcp_docs_ping", "d-"+name, tc.class, tc.agentID); err == nil {
			t.Errorf("%s ran an mcp tool", name)
		}
	}
	if calls := starter.session("docs").callCount(); calls != 1 {
		t.Errorf("denied calls reached the server: %d", calls)
	}
	if got := len(harness.promptedTools()); got != prompts {
		t.Errorf("denied calls raised %d approval prompts", got-prompts)
	}
}

func TestMCPRuntimeNeverStartsUnapprovedOrChangedServers(t *testing.T) {
	harness := newMCPHarness(t, []config.MCPServer{mcpTestServer("docs")})
	starter := newFixtureStarter(map[string][]string{"docs": {"ping"}})
	started := harness.start(t, starter)

	if starter.started("docs") != 0 {
		t.Fatalf("an unapproved server was started %d times", starter.started("docs"))
	}
	if slices.Contains(harness.toolNames(), "mcp_docs_ping") {
		t.Error("tools of an unapproved server are registered")
	}
	if got := findingCodes(started.check.Checker.Check(t.Context())); got["docs"] != "mcp_trust_required" {
		t.Fatalf("findings = %v, want mcp_trust_required for docs", got)
	}
	_ = started.Close(t.Context())

	harness.approveServer(t, "docs")
	harness.cfg.MCP.Servers[0].Args = []string{"--stdio", "--extra"}
	changed := harness.start(t, starter)
	if starter.started("docs") != 0 {
		t.Fatal("a server whose configuration changed after approval was started")
	}
	if got := findingCodes(changed.check.Checker.Check(t.Context())); got["docs"] != "mcp_trust_required" {
		t.Fatalf("findings after change = %v", got)
	}
	_ = changed.Close(t.Context())

	harness.approveServer(t, "docs")
	approved := harness.start(t, starter)
	if starter.started("docs") != 1 {
		t.Fatalf("an approved server was started %d times, want 1", starter.started("docs"))
	}
	if got := approved.check.Checker.Check(t.Context()); len(got) != 1 || got[0].Code != "ok" {
		t.Fatalf("findings after approval = %+v", got)
	}
}

func TestMCPRuntimeIsolatesFailingServers(t *testing.T) {
	servers := []config.MCPServer{mcpTestServer("crashy"), mcpTestServer("steady"), mcpTestServer("stranger")}
	harness := newMCPHarness(t, servers)
	harness.approveServer(t, "crashy")
	harness.approveServer(t, "steady")
	starter := newFixtureStarter(map[string][]string{"steady": {"ping"}, "stranger": {"ping"}})
	starter.failing["crashy"] = true
	started := harness.start(t, starter)

	got := findingCodes(started.check.Checker.Check(t.Context()))
	want := map[string]string{"crashy": "mcp_server_unavailable", "stranger": "mcp_trust_required"}
	if len(got) != len(want) || got["crashy"] != want["crashy"] || got["stranger"] != want["stranger"] {
		t.Fatalf("findings = %v, want %v", got, want)
	}
	if !slices.Contains(harness.toolNames(), "mcp_steady_ping") {
		t.Fatal("a healthy server lost its tools because a sibling failed")
	}
	if err := harness.callTool(t.Context(), "mcp_steady_ping", "iso-1", principal.ClassOwner); err != nil {
		t.Fatalf("healthy server call: %v", err)
	}

	starter.session("steady").breakChild()
	if err := harness.callTool(t.Context(), "mcp_steady_ping", "iso-2", principal.ClassOwner); err == nil {
		t.Fatal("call to a crashed server succeeded")
	}
	got = findingCodes(started.check.Checker.Check(t.Context()))
	if got["steady"] != "mcp_server_unavailable" {
		t.Fatalf("findings after crash = %v, want mcp_server_unavailable for steady", got)
	}
	if !slices.Contains(harness.toolNames(), "read_file") {
		t.Error("built-in tools disappeared")
	}
}

func TestMCPRuntimeCloseLeavesNoSessionOrGoroutine(t *testing.T) {
	servers := []config.MCPServer{mcpTestServer("one"), mcpTestServer("two")}
	harness := newMCPHarness(t, servers)
	harness.approveServer(t, "one")
	harness.approveServer(t, "two")
	baseline := runtime.NumGoroutine()
	starter := newFixtureStarter(map[string][]string{"one": {"ping"}, "two": {"ping"}})
	started := harness.start(t, starter)
	if starter.openSessions() != 2 {
		t.Fatalf("open sessions = %d, want 2", starter.openSessions())
	}
	if err := started.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if starter.openSessions() != 0 {
		t.Errorf("open sessions after Close = %d", starter.openSessions())
	}
	for _, name := range harness.toolNames() {
		if strings.HasPrefix(name, "mcp_") {
			t.Errorf("tool %s outlived Close", name)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+2 {
		t.Errorf("goroutines after Close = %d, baseline %d", got, baseline)
	}
	var nilRuntime *mcpRuntime
	if err := nilRuntime.Close(t.Context()); err != nil {
		t.Errorf("nil runtime Close: %v", err)
	}
}

func TestMCPRuntimeRequiresInputs(t *testing.T) {
	if _, err := startMCPRuntime(t.Context(), nil); err == nil {
		t.Error("nil options accepted")
	}
	if _, err := startMCPRuntime(t.Context(), &mcpRuntimeOptions{}); err == nil {
		t.Error("empty options accepted")
	}
}

func TestMCPServerDeclaringACapabilityOutsideTheRegistryRegistersNoTools(t *testing.T) {
	server := mcpTestServer("docs")
	server.Capabilities = []string{"fs.read"}
	harness := newMCPHarness(t, []config.MCPServer{server})
	harness.approveServer(t, "docs")
	starter := newFixtureStarter(map[string][]string{"docs": {"ping"}})
	started := harness.start(t, starter)

	if starter.started("docs") != 0 {
		t.Fatalf("sessions started = %d, want the server refused before any session", starter.started("docs"))
	}
	if slices.Contains(harness.toolNames(), "mcp_docs_ping") {
		t.Fatalf("broker tools = %v, want no tool from a server with an unregistered capability", harness.toolNames())
	}
	statuses := started.manager.Statuses()
	if len(statuses) != 1 || statuses[0].Code != mcp.ErrCapabilityUnavailable {
		t.Fatalf("statuses = %+v, want the server marked capability_unavailable", statuses)
	}
}

func TestMCPCapabilityCheckerRejectsCapabilitiesOutsideTheRegistry(t *testing.T) {
	check := capabilitytest.Report(t, "workspace-read").Require
	if err := check([]string{"workspace-read"}); err != nil {
		t.Errorf("available capability rejected: %v", err)
	}
	if err := check([]string{"fs.read"}); err == nil {
		t.Error("capability outside the registry accepted")
	}
}

func mcpPersonaConfig(t *testing.T, tools string) *config.Config {
	t.Helper()
	path := writeAgentsConfig(t, "storage:\n  path: "+t.TempDir()+"\n"+mcpServersYAML("--x")+`agents:
  definitions:
    - id: main
      tools: `+tools+`
`)
	result, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return result.Config
}

func TestPersonasSeeMCPToolsOnlyWhenTheyListThem(t *testing.T) {
	registry, err := buildAgentRegistry(mcpPersonaConfig(t, "[read_file, mcp_docs, mcp_remote_search]"))
	if err != nil {
		t.Fatalf("buildAgentRegistry: %v", err)
	}
	main, _ := registry.Lookup("main")
	if !slices.Equal(main.Tools, []string{"read_file", "mcp_docs", "mcp_remote_search"}) {
		t.Fatalf("main tools = %v", main.Tools)
	}
	engineer, _ := registry.Lookup("engineer")
	for _, tool := range engineer.Tools {
		if strings.HasPrefix(tool, "mcp_") {
			t.Errorf("persona without mcp entries lists %s", tool)
		}
	}

	out, err := execute(t, "agents", "show", "main", "--config", writeShowConfig(t, "[read_file, mcp_docs]"))
	if err != nil || !strings.Contains(out, "Tools: read_file, mcp_docs\n") {
		t.Fatalf("agents show = %q, %v", out, err)
	}
}

func writeShowConfig(t *testing.T, tools string) string {
	t.Helper()
	return writeAgentsConfig(t, "storage:\n  path: "+t.TempDir()+"\n"+mcpServersYAML("--x")+`agents:
  definitions:
    - id: main
      tools: `+tools+`
`)
}

func TestUnknownMCPServerInPersonaFailsBootWithPersonaInvalid(t *testing.T) {
	for _, tools := range []string{"[mcp_ghost]", "[read_file, mcp_ghost_search]"} {
		_, err := buildAgentRegistry(mcpPersonaConfig(t, tools))
		if code, ok := auraagent.CodeOf(err); !ok || code != auraagent.ErrorCodePersonaInvalid {
			t.Errorf("tools %s: error = %v (code %q, %v), want persona_invalid", tools, err, code, ok)
		}
	}
}

func mcpTierConfig(t *testing.T, section string) *config.Config {
	t.Helper()
	path := writeAgentsConfig(t, "storage:\n  path: "+t.TempDir()+"\n"+mcpServersYAML("--x")+`agents:
  definitions:
    - id: lobby
      description: Front desk
      instructions: Help visitors.
      audience: trusted
      tools: [read_file, mcp_docs]
      `+section+`
`)
	result, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return result.Config
}

func TestMCPEntriesInPublicToolsFailBoot(t *testing.T) {
	for _, section := range []string{"public_tools: [mcp_docs]", "public_tools: [mcp_docs_search]"} {
		_, err := buildAgentRegistry(mcpTierConfig(t, section))
		if err == nil {
			t.Errorf("%s accepted", section)
		} else if code, ok := auraagent.CodeOf(err); !ok || code != auraagent.ErrorCodePersonaInvalid {
			t.Errorf("%s: error = %v, want persona_invalid", section, err)
		}
	}
}

func TestMCPEntriesInPrincipalToolsNeedToolsCoverageAtBoot(t *testing.T) {
	for _, section := range []string{"principal_tools: [mcp_docs]", "principal_tools: [read_file, mcp_docs_search]"} {
		registry, err := buildAgentRegistry(mcpTierConfig(t, section))
		if err != nil {
			t.Errorf("%s rejected: %v", section, err)
			continue
		}
		if _, ok := registry.Lookup("lobby"); !ok {
			t.Errorf("%s: persona missing", section)
		}
	}
	for _, section := range []string{"principal_tools: [mcp_docs_search, mcp_files]", "principal_tools: [mcp_ghost]"} {
		_, err := buildAgentRegistry(mcpTierConfig(t, section))
		if code, ok := auraagent.CodeOf(err); !ok || code != auraagent.ErrorCodePersonaInvalid {
			t.Errorf("%s: error = %v, want persona_invalid", section, err)
		}
	}
}

func TestMCPWarnsAboutListedToolsAServerDoesNotExpose(t *testing.T) {
	harness := newMCPHarness(t, []config.MCPServer{mcpTestServer("docs")})
	harness.approveServer(t, "docs")
	started := harness.start(t, newFixtureStarter(map[string][]string{"docs": {"ping"}}))
	var records []string
	logger := slog.New(&recordingHandler{records: &records})
	started.warnUnexposedPersonaTools(t.Context(), logger, []auraagent.Definition{
		{ID: "ops", Tools: []string{"read_file", "mcp_docs", "mcp_docs_ping", "mcp_docs_missing"}},
	})
	if len(records) != 1 || !strings.Contains(records[0], "persona=ops") || !strings.Contains(records[0], "tool=mcp_docs_missing") {
		t.Fatalf("warnings = %v, want one for mcp_docs_missing", records)
	}
}

type recordingHandler struct {
	records *[]string
	attrs   []slog.Attr
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Level != slog.LevelWarn {
		return nil
	}
	parts := []string{record.Message}
	record.Attrs(func(attr slog.Attr) bool {
		parts = append(parts, attr.Key+"="+attr.Value.String())
		return true
	})
	*h.records = append(*h.records, strings.Join(parts, " "))
	return nil
}

func (h *recordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &recordingHandler{records: h.records, attrs: append(slices.Clone(h.attrs), attrs...)}
}

func (h *recordingHandler) WithGroup(string) slog.Handler { return h }

func TestConfigsWithoutMCPServersAreUnchanged(t *testing.T) {
	for name, section := range map[string]string{"no section": "", "empty servers": "mcp:\n  servers: []\n"} {
		t.Run(name, func(t *testing.T) {
			path := writeAgentsConfig(t, "storage:\n  path: "+t.TempDir()+"\n"+section)
			result, err := config.Load(path)
			if err != nil {
				t.Fatalf("config.Load: %v", err)
			}
			cfg := result.Config

			registry, err := buildAgentRegistry(cfg)
			if err != nil {
				t.Fatalf("buildAgentRegistry: %v", err)
			}
			baseline, err := auraagent.Build(nil, toolsbuiltin.DefinitionNames(), []string{"primary"})
			if err != nil {
				t.Fatalf("auraagent.Build: %v", err)
			}
			got, want := registry.Definitions(), baseline.Definitions()
			if len(got) != len(want) {
				t.Fatalf("personas = %d, want %d", len(got), len(want))
			}
			for i := range got {
				if got[i].Fingerprint() != want[i].Fingerprint() || got[i].EvalDigest() != want[i].EvalDigest() || !slices.Equal(got[i].Tools, want[i].Tools) {
					t.Errorf("persona %s differs from the baseline", got[i].ID)
				}
			}

			harness := newMCPHarness(t, nil)
			harness.cfg.MCP = cfg.MCP
			if mcpServersConfigured(&harness.cfg) {
				t.Fatal("a config without enabled servers reports mcp servers")
			}
			wantTools := []string{"code_execute", "email_send", "exec", "handoff_to_owner", "kb_search", "list_dir", "read_file", "web_fetch", "web_search", "workflow_cancel", "workflow_list", "workflow_propose", "workflow_save_routine", "workflow_start", "workflow_status", "write_file"}
			if got := harness.toolNames(); !slices.Equal(got, wantTools) {
				t.Fatalf("broker tools = %v, want %v", got, wantTools)
			}

			healthRegistry, err := buildHealthRegistry(cfg, nil, func() (bool, string) { return true, "ok" }, func() (health.ProcessStatus, bool) { return health.ProcessStatus{}, false })
			if err != nil {
				t.Fatalf("buildHealthRegistry: %v", err)
			}
			for _, finding := range healthRegistry.Evaluate(t.Context()) {
				if strings.HasPrefix(finding.Component, "mcp") {
					t.Errorf("health reports %+v without any mcp server", finding)
				}
			}
		})
	}
}
