//go:build linux

package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/codeexec"
	"github.com/anggasct/aura/internal/modules/handoff"
	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/modules/search"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/secret/secrettest"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/storage"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

type gateFixture struct {
	executor  *runtimeadk.BrokerExecutor
	workspace string
	prompts   int
}

func registerFullCatalogProviders(t *testing.T, broker *toolbroker.Broker) {
	t.Helper()
	noop := func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
		return toolbroker.ToolResult{Class: toolbroker.ResultOK, Output: json.RawMessage(`{}`)}, nil
	}
	wfProvider, err := workflow.NewToolProvider(goldenWorkflowController{})
	if err != nil {
		t.Fatalf("workflow.NewToolProvider: %v", err)
	}
	for _, provider := range []toolbroker.ToolProvider{
		codeexec.NewToolProvider(noop, nil),
		knowledge.NewToolProvider(nil),
		handoff.NewToolProvider(nil),
		wfProvider,
	} {
		if err := broker.RegisterProvider(provider); err != nil {
			t.Fatalf("RegisterProvider: %v", err)
		}
	}
}

func newGateFixture(t *testing.T, report capability.Report, approve bool) *gateFixture {
	t.Helper()
	fixture := &gateFixture{workspace: t.TempDir()}
	if err := os.WriteFile(filepath.Join(fixture.workspace, "note.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	dataRoot := t.TempDir()
	sqlitetest.Seed(t, filepath.Join(dataRoot, "aura.db"), Schemas()...)
	db, err := sqlite.Open(t.Context(), filepath.Join(dataRoot, "aura.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	session := turn.Session{ID: "session-1", OwnerID: "owner-1"}
	if err := turnstore.NewSessionService(db).Create(t.Context(), &session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	cfg := appconfig.Default()
	cfg.Tools.Workspace = fixture.workspace
	executor, err := newTestToolExecutor(&toolExecutorInput{
		result:       &appconfig.LoadResult{Config: &cfg, CapabilityReport: report},
		db:           db,
		artifactRoot: dataRoot,
		logger:       slog.New(slog.DiscardHandler),
		wiring: &toolWiring{
			decider: func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) {
				fixture.prompts++
				return approve, nil
			},
		},
		events: turn.NewEventBus(slog.New(slog.DiscardHandler)),
	})
	if err != nil {
		t.Fatalf("newTestToolExecutor: %v", err)
	}
	registerFullCatalogProviders(t, executor.Broker())
	fixture.executor = executor
	return fixture
}

func (f *gateFixture) call(tool, arguments string, capabilities ...string) (json.RawMessage, error) {
	return f.executor.Execute(context.Background(), &runtimeadk.BuiltinToolRequest{
		RequestID: "call-" + tool, TurnID: "turn-1", SessionID: "session-1", PrincipalID: "owner-1",
		ToolName: tool, ToolVersion: "v1", Arguments: json.RawMessage(arguments),
		Capabilities: capabilities, Trust: string(approval.TrustDerivedUntrusted),
		IdempotencyKey: "test/" + tool, EventInvocation: "inv-1", EventBranch: "main", EventAuthor: "agent",
	})
}

func offered(executor *runtimeadk.BrokerExecutor) []string {
	definitions := executor.Definitions()
	names := make([]string, 0, len(definitions))
	for i := range definitions {
		names = append(names, definitions[i].Name)
	}
	slices.Sort(names)
	return names
}

var ungatedTools = []string{"email_send", "handoff_to_owner", "kb_search", "workflow_cancel", "workflow_list", "workflow_propose", "workflow_save_routine", "workflow_start", "workflow_status"}

func TestExecutorOffersOnlyToolsWhoseCapabilityIsActive(t *testing.T) {
	cases := []struct {
		name    string
		enabled []string
		gated   []string
	}{
		{name: "no capability enabled", enabled: nil, gated: nil},
		{name: "workspace read", enabled: []string{"workspace-read"}, gated: []string{"list_dir", "read_file"}},
		{name: "workspace write", enabled: []string{"workspace-write"}, gated: []string{"write_file"}},
		{name: "exec", enabled: []string{"exec-linux"}, gated: []string{"exec"}},
		{name: "public web", enabled: []string{"public-web"}, gated: []string{"web_fetch"}},
		{name: "provider search", enabled: []string{"provider-search"}, gated: []string{"web_search"}},
		{name: "code interpreter", enabled: []string{"code-interpreter"}, gated: []string{"code_execute"}},
		{name: "everything", enabled: []string{"exec-linux", "workspace-read", "workspace-write", "public-web", "provider-search", "code-interpreter"}, gated: []string{"code_execute", "exec", "list_dir", "read_file", "web_fetch", "web_search", "write_file"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newGateFixture(t, capabilitytest.Report(t, tc.enabled...), true)
			want := slices.Sorted(slices.Values(append(slices.Clone(ungatedTools), tc.gated...)))
			if got := offered(fixture.executor); !slices.Equal(got, want) {
				t.Fatalf("offered = %v, want %v", got, want)
			}
		})
	}
}

func TestExecutorWithTheZeroReportOffersOnlyUngatedTools(t *testing.T) {
	fixture := newGateFixture(t, capability.Report{}, true)
	if got, want := offered(fixture.executor), ungatedTools; !slices.Equal(got, want) {
		t.Fatalf("offered = %v, want %v", got, want)
	}
}

func TestExecutorOmitsEnabledCapabilitiesThatAreNotAvailable(t *testing.T) {
	registry, err := capability.BuiltinRegistry()
	if err != nil {
		t.Fatalf("BuiltinRegistry: %v", err)
	}
	build, err := capability.NewBuild(string(capability.ProfileExecLinux), []string{"exec-linux", "workspace-read"}, "linux")
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	report, _ := registry.Resolve(build, []string{"exec-linux", "workspace-read"}, capability.Dependencies{})
	fixture := newGateFixture(t, report, true)
	want := slices.Sorted(slices.Values(append(slices.Clone(ungatedTools), "list_dir", "read_file")))
	if got := offered(fixture.executor); !slices.Equal(got, want) {
		t.Fatalf("offered = %v, want %v", got, want)
	}
}

func TestDirectCallsToUnavailableToolsAreDeniedWithoutEffect(t *testing.T) {
	cases := []struct {
		tool       string
		arguments  string
		capability string
		effect     func(workspace string) bool
	}{
		{tool: "exec", arguments: `{"command":["touch","exec-marker"]}`, capability: "exec-linux", effect: markerExists("exec-marker")},
		{tool: "read_file", arguments: `{"path":"note.txt"}`, capability: "workspace-read"},
		{tool: "list_dir", arguments: `{"path":"."}`, capability: "workspace-read"},
		{tool: "write_file", arguments: `{"path":"written.txt","content":"x"}`, capability: "workspace-write", effect: markerExists("written.txt")},
		{tool: "web_fetch", arguments: `{"url":"https://example.com"}`, capability: "public-web"},
		{tool: "web_search", arguments: `{"query":"aura"}`, capability: "provider-search"},
		{tool: "code_execute", arguments: `{"runtime":"python","source":"open('marker', 'w')"}`, capability: "code-interpreter", effect: markerExists("marker")},
	}
	for _, tc := range cases {
		reports := map[string]capability.Report{
			"nothing enabled":    capabilitytest.Report(t),
			"zero report":        {},
			"only other enabled": capabilitytest.Report(t, otherCapability(tc.capability)),
		}
		for reportName, report := range reports {
			t.Run(tc.tool+"/"+reportName, func(t *testing.T) {
				fixture := newGateFixture(t, report, true)
				_, err := fixture.call(tc.tool, tc.arguments, tc.capability)
				if !fault.HasCode(err, toolbroker.ResultCapabilityUnavailable) {
					t.Fatalf("call error = %v, want capability_unavailable", err)
				}
				if fixture.prompts != 0 {
					t.Fatalf("approval prompts = %d, want none", fixture.prompts)
				}
				if tc.effect != nil && tc.effect(fixture.workspace) {
					t.Fatal("the denied call left an effect in the workspace")
				}
			})
		}
	}
}

func otherCapability(name string) string {
	if name == "workspace-read" {
		return "public-web"
	}
	return "workspace-read"
}

func markerExists(name string) func(workspace string) bool {
	return func(workspace string) bool {
		_, err := os.Stat(filepath.Join(workspace, name))
		return err == nil
	}
}

func TestEnabledCapabilityRunsItsToolAsBefore(t *testing.T) {
	fixture := newGateFixture(t, capabilitytest.Report(t, "workspace-read"), true)
	output, err := fixture.call("read_file", `{"path":"note.txt"}`, "workspace-read")
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	var result struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(output, &result); err != nil || result.Content != "hello" {
		t.Fatalf("read_file output = %s, %v; want hello", output, err)
	}
	if fixture.prompts != 0 {
		t.Fatalf("approval prompts = %d, want none for a read", fixture.prompts)
	}
}

func TestEnabledExecStillNeedsApproval(t *testing.T) {
	fixture := newGateFixture(t, capabilitytest.Report(t, "exec-linux"), false)
	if !slices.Contains(offered(fixture.executor), "exec") {
		t.Fatal("exec is not offered with exec-linux enabled")
	}
	_, err := fixture.call("exec", `{"command":["touch","exec-marker"]}`, "exec-linux")
	if !fault.HasCode(err, toolbroker.ResultApprovalRequired) {
		t.Fatalf("exec error = %v, want approval_required", err)
	}
	if fixture.prompts != 1 {
		t.Fatalf("approval prompts = %d, want one", fixture.prompts)
	}
	if markerExists("exec-marker")(fixture.workspace) {
		t.Fatal("a rejected exec left an effect")
	}
}

func TestRequestClaimingACapabilityTheHostLacksIsDenied(t *testing.T) {
	fixture := newGateFixture(t, capabilitytest.Report(t, "workspace-read"), true)
	_, err := fixture.call("read_file", `{"path":"note.txt"}`, "workspace-read", "exec-linux")
	if !fault.HasCode(err, toolbroker.ResultCapabilityUnavailable) {
		t.Fatalf("read_file error = %v, want capability_unavailable", err)
	}
}

func TestBuiltinToolExecutorComposesBrokerAdapters(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "note.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	db, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "aura.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := appconfig.Default()
	cfg.Tools.Workspace = workspace
	var observations []toolbroker.Observation
	executor, err := newTestToolExecutor(&toolExecutorInput{
		result:       &appconfig.LoadResult{Config: &cfg, CapabilityReport: capabilitytest.All(t)},
		db:           db,
		artifactRoot: t.TempDir(),
		logger:       slog.New(slog.DiscardHandler),
		wiring: &toolWiring{
			observer: func(_ context.Context, observation toolbroker.Observation) {
				observations = append(observations, observation)
			},
		},
		events: turn.NewEventBus(slog.New(slog.DiscardHandler)),
	})
	if err != nil {
		t.Fatalf("newTestToolExecutor: %v", err)
	}
	registerFullCatalogProviders(t, executor.Broker())
	definitions := executor.Definitions()
	if len(definitions) != 16 {
		t.Fatalf("builtin definitions = %d, want 16", len(definitions))
	}
	wantDefinitions := []string{"code_execute@v1", "email_send@v1", "exec@v1", "handoff_to_owner@v1", "kb_search@v1", "list_dir@v1", "read_file@v1", "web_fetch@v1", "web_search@v1", "workflow_cancel@v1", "workflow_list@v1", "workflow_propose@v1", "workflow_save_routine@v1", "workflow_start@v1", "workflow_status@v1", "write_file@v1"}
	for i, definition := range definitions {
		if key := definition.Name + "@" + definition.Version; key != wantDefinitions[i] {
			t.Errorf("definition[%d] = %q, want %q", i, key, wantDefinitions[i])
		}
	}
	output, err := executor.Execute(t.Context(), &runtimeadk.BuiltinToolRequest{
		RequestID:    "call-1",
		TurnID:       "turn-1",
		SessionID:    "session-1",
		PrincipalID:  "owner-1",
		ToolName:     "read_file",
		ToolVersion:  "v1",
		Arguments:    json.RawMessage(`{"path":"note.txt"}`),
		Capabilities: []string{"workspace-read"},
		Trust:        string(approval.TrustDerivedUntrusted),
	})
	if err != nil {
		t.Fatalf("execute read_file: %v", err)
	}
	var result struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.Content != "hello" {
		t.Fatalf("content = %q, want hello", result.Content)
	}
	if len(observations) != 1 {
		t.Fatalf("observations = %d, want 1", len(observations))
	}
	observation := observations[0]
	if observation.ToolName != "read_file" || observation.ToolVersion != "v1" || observation.Class != toolbroker.ResultOK {
		t.Fatalf("observation = %+v", observation)
	}
	if observation.Duration <= 0 || observation.OutputBytes == 0 {
		t.Fatalf("observation lacks duration/bytes: %+v", observation)
	}
}

func TestBuiltinToolExecutorSpillsOversizedResultsToArtifacts(t *testing.T) {
	workspace := t.TempDir()
	big := strings.Repeat("a", 512)
	if err := os.WriteFile(filepath.Join(workspace, "big.txt"), []byte(big), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	db := sqlitetest.OpenMigrated(t, Schemas()...)
	sess := turn.Session{ID: "session-1", OwnerID: "owner-1"}
	if err := turnstore.NewSessionService(db).Create(t.Context(), &sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	cfg := appconfig.Default()
	cfg.Tools.Workspace = workspace
	cfg.Tools.MaxInlineResultBytes = 256
	executor, err := newTestToolExecutor(&toolExecutorInput{
		result:       &appconfig.LoadResult{Config: &cfg, CapabilityReport: capabilitytest.All(t)},
		db:           db,
		artifactRoot: t.TempDir(),
		logger:       slog.New(slog.DiscardHandler),
		wiring:       &toolWiring{},
		events:       turn.NewEventBus(slog.New(slog.DiscardHandler)),
	})
	if err != nil {
		t.Fatalf("newTestToolExecutor: %v", err)
	}
	output, err := executor.Execute(t.Context(), &runtimeadk.BuiltinToolRequest{
		RequestID:       "call-1",
		TurnID:          "turn-1",
		SessionID:       "session-1",
		PrincipalID:     "owner-1",
		ToolName:        "read_file",
		ToolVersion:     "v1",
		Arguments:       json.RawMessage(`{"path":"big.txt"}`),
		Capabilities:    []string{"workspace-read"},
		Trust:           string(approval.TrustDerivedUntrusted),
		IdempotencyKey:  "test/spill/1",
		EventInvocation: "inv-1",
		EventBranch:     "main",
		EventAuthor:     "agent",
	})
	if err != nil {
		t.Fatalf("execute read_file: %v", err)
	}
	var envelope struct {
		ArtifactID string `json:"artifact_id"`
		Digest     string `json:"digest"`
		SizeBytes  int64  `json:"size_bytes"`
		Truncated  bool   `json:"truncated"`
		Body       string `json:"body"`
	}
	if err := json.Unmarshal(output, &envelope); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, output)
	}
	if envelope.ArtifactID == "" {
		t.Fatalf("oversized result did not spill to an artifact: %s", output)
	}
	if envelope.Truncated {
		t.Fatalf("artifact spill must not be marked truncated: %s", output)
	}
	if envelope.Digest == "" || envelope.SizeBytes == 0 {
		t.Fatalf("envelope lacks digest/size: %s", output)
	}
	if envelope.Body != "" {
		t.Fatalf("artifact spill must not inline the body: %s", output)
	}
}

func TestBuiltinToolExecutorRedactsConfiguredSecretAcrossBoundaries(t *testing.T) {
	const canary = "tool-secret-7f3a9d1c"
	t.Setenv("AURA_TEST_TOOL_SECRET", canary)
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "secret.txt"), []byte(strings.Repeat(canary, 128)), 0o600); err != nil {
		t.Fatalf("write secret fixture: %v", err)
	}
	dataRoot := t.TempDir()
	sqlitetest.Seed(t, filepath.Join(dataRoot, "aura.db"), Schemas()...)
	db, err := sqlite.Open(t.Context(), filepath.Join(dataRoot, "aura.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	session := turn.Session{ID: "session-secret", OwnerID: "owner-1"}
	if err := turnstore.NewSessionService(db).Create(t.Context(), &session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	cfg := appconfig.Default()
	cfg.Tools.Workspace = workspace
	cfg.Tools.WebSearch.Provider = "brave"
	cfg.Tools.WebSearch.CredentialRef = secrettest.Ref("env://AURA_TEST_TOOL_SECRET")
	cfg.Tools.MaxInlineResultBytes = 512
	var prompted *toolbroker.ApprovalPrompt
	executor, err := newTestToolExecutor(&toolExecutorInput{
		result:       &appconfig.LoadResult{Config: &cfg, CapabilityReport: capabilitytest.All(t)},
		db:           db,
		artifactRoot: dataRoot,
		logger:       slog.New(slog.DiscardHandler),
		wiring: &toolWiring{
			decider: func(_ context.Context, prompt *toolbroker.ApprovalPrompt) (bool, error) {
				prompted = prompt
				return false, nil
			},
		},
		events: turn.NewEventBus(slog.New(slog.DiscardHandler)),
	})
	if err != nil {
		t.Fatalf("newTestToolExecutor: %v", err)
	}
	_, err = executor.Execute(t.Context(), &runtimeadk.BuiltinToolRequest{
		RequestID:       "approval-secret",
		TurnID:          "turn-secret",
		SessionID:       "session-secret",
		PrincipalID:     "owner-1",
		ToolName:        "write_file",
		ToolVersion:     "v1",
		Arguments:       json.RawMessage(`{"path":"out.txt","content":"tool-secret-7f3a9d1c"}`),
		Capabilities:    []string{"workspace-write"},
		Trust:           string(approval.TrustOwnerInput),
		IdempotencyKey:  "approval-secret-1",
		EventInvocation: "inv-secret",
		EventBranch:     "main",
		EventAuthor:     "agent",
	})
	class, ok := fault.CodeOf[toolbroker.ResultClass](err)
	if !ok || class != toolbroker.ResultApprovalRequired {
		t.Fatalf("approval error = %v, want approval_required", err)
	}
	if prompted == nil || strings.Contains(prompted.Arguments, canary) || !strings.Contains(prompted.Arguments, "[REDACTED]") {
		t.Fatalf("approval prompt leaked configured secret: %+v", prompted)
	}

	diagnosticExecutor, err := newTestToolExecutor(&toolExecutorInput{
		result:       &appconfig.LoadResult{Config: &cfg, CapabilityReport: capabilitytest.All(t)},
		db:           db,
		artifactRoot: dataRoot,
		logger:       slog.New(slog.DiscardHandler),
		wiring: &toolWiring{
			decider: func(_ context.Context, _ *toolbroker.ApprovalPrompt) (bool, error) {
				return false, errors.New("approval surface failed: " + canary)
			},
		},
		events: turn.NewEventBus(slog.New(slog.DiscardHandler)),
	})
	if err != nil {
		t.Fatalf("new diagnostic executor: %v", err)
	}
	_, err = diagnosticExecutor.Execute(t.Context(), &runtimeadk.BuiltinToolRequest{
		RequestID:      "diagnostic-secret",
		TurnID:         "turn-secret",
		SessionID:      "session-secret",
		PrincipalID:    "owner-1",
		ToolName:       "write_file",
		ToolVersion:    "v1",
		Arguments:      json.RawMessage(`{"path":"out.txt","content":"safe"}`),
		Capabilities:   []string{"workspace-write"},
		Trust:          string(approval.TrustOwnerInput),
		IdempotencyKey: "diagnostic-secret-1",
	})
	if err == nil {
		t.Fatal("diagnostic executor unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("diagnostic leaked configured secret: %v", err)
	}

	output, err := executor.Execute(t.Context(), &runtimeadk.BuiltinToolRequest{
		RequestID:      "output-secret",
		TurnID:         "turn-secret",
		SessionID:      "session-secret",
		PrincipalID:    "owner-1",
		ToolName:       "read_file",
		ToolVersion:    "v1",
		Arguments:      json.RawMessage(`{"path":"secret.txt"}`),
		Capabilities:   []string{"workspace-read"},
		Trust:          string(approval.TrustDerivedUntrusted),
		IdempotencyKey: "output-secret-1",
	})
	if err != nil {
		t.Fatalf("read secret fixture: %v", err)
	}
	var envelope struct {
		ArtifactID string `json:"artifact_id"`
	}
	if err := json.Unmarshal(output, &envelope); err != nil || envelope.ArtifactID == "" {
		t.Fatalf("secret read did not spill to artifact: %s (%v)", output, err)
	}
	artifacts := storage.NewArtifactStore(db, dataRoot, int64(cfg.Storage.ArtifactQuota))
	reader, _, err := artifacts.Open(t.Context(), envelope.ArtifactID)
	if err != nil {
		t.Fatalf("read spilled artifact: %v", err)
	}
	defer func() { _ = reader.Close() }()
	spilled, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read artifact bytes: %v", err)
	}
	if strings.Contains(string(spilled), canary) || !strings.Contains(string(spilled), "[REDACTED]") {
		t.Fatalf("spilled artifact leaked configured secret: %s", spilled)
	}
}

func TestWiringRegistersEveryBuiltinAdapter(t *testing.T) {
	dataRoot := t.TempDir()
	sqlitetest.Seed(t, filepath.Join(dataRoot, "aura.db"), Schemas()...)
	db, err := sqlite.Open(t.Context(), filepath.Join(dataRoot, "aura.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := appconfig.Default()
	cfg.Tools.Workspace = t.TempDir()
	executor, err := newTestToolExecutor(&toolExecutorInput{
		result:       &appconfig.LoadResult{Config: &cfg, CapabilityReport: capabilitytest.All(t)},
		db:           db,
		artifactRoot: dataRoot,
		logger:       slog.New(slog.DiscardHandler),
		wiring:       &toolWiring{},
		events:       turn.NewEventBus(slog.New(slog.DiscardHandler)),
	})
	if err != nil {
		t.Fatalf("newTestToolExecutor: %v", err)
	}
	want := []string{"email_send", "exec", "list_dir", "read_file", "web_fetch", "web_search", "write_file"}
	if got := offered(executor); !slices.Equal(got, want) {
		t.Fatalf("registered core tools = %v, want %v", got, want)
	}
}

func TestWiringConnectsEffectJournalAndEventPublisher(t *testing.T) {
	workspace := t.TempDir()
	dataRoot := t.TempDir()
	sqlitetest.Seed(t, filepath.Join(dataRoot, "aura.db"), Schemas()...)
	db, err := sqlite.Open(t.Context(), filepath.Join(dataRoot, "aura.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sess := turn.Session{ID: "session-1", OwnerID: "owner-1"}
	if err := turnstore.NewSessionService(db).Create(t.Context(), &sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	cfg := appconfig.Default()
	cfg.Tools.Workspace = workspace
	bus := turn.NewEventBus(slog.New(slog.DiscardHandler))
	sub := make(chan *turn.Event, 8)
	if err := bus.Subscribe(func(ev *turn.Event) {
		sub <- ev
	}); err != nil {
		t.Fatalf("subscribe bus: %v", err)
	}
	executor, err := newTestToolExecutor(&toolExecutorInput{
		result:       &appconfig.LoadResult{Config: &cfg, CapabilityReport: capabilitytest.All(t)},
		db:           db,
		artifactRoot: dataRoot,
		logger:       slog.New(slog.DiscardHandler),
		wiring: &toolWiring{
			decider: func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) {
				return true, nil
			},
		},
		events: bus,
	})
	if err != nil {
		t.Fatalf("newTestToolExecutor: %v", err)
	}
	if _, err := executor.Execute(t.Context(), &runtimeadk.BuiltinToolRequest{
		RequestID:       "wiring-effect-1",
		TurnID:          "turn-1",
		SessionID:       "session-1",
		PrincipalID:     "owner-1",
		ToolName:        "write_file",
		ToolVersion:     "v1",
		Arguments:       json.RawMessage(`{"path":"out.txt","content":"hello"}`),
		Capabilities:    []string{"workspace-write"},
		Trust:           string(approval.TrustOwnerInput),
		IdempotencyKey:  "wiring/effect/1",
		EventInvocation: "inv-1",
		EventBranch:     "main",
		EventAuthor:     "agent",
	}); err != nil {
		t.Fatalf("execute effectful tool: %v", err)
	}
	var intents int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM effect_intent`).Scan(&intents); err != nil {
		t.Fatalf("count effect intents: %v", err)
	}
	if intents != 1 {
		t.Fatalf("effect intents = %d, want 1", intents)
	}
	select {
	case ev := <-sub:
		if ev.Kind != turn.EventKindToolRequested {
			t.Fatalf("published event kind = %q, want %q", ev.Kind, turn.EventKindToolRequested)
		}
	default:
		t.Fatal("expected tool.requested event on bus")
	}
}

func TestEveryConfiguredWebSearchProviderBuildsAnAdapter(t *testing.T) {
	for _, provider := range search.Providers() {
		t.Run(provider, func(t *testing.T) {
			cfg := appconfig.Default()
			cfg.Tools.Workspace = t.TempDir()
			cfg.Tools.WebSearch = search.Config{
				Provider:      provider,
				CredentialRef: search.DefaultCredentialRef(provider),
				MaxResults:    5,
			}
			if provider == search.ProviderSearXNG {
				cfg.Tools.WebSearch.Endpoint = "https://search.example.com/search"
			}
			db := sqlitetest.OpenMigrated(t, Schemas()...)
			executor, err := newTestToolExecutor(&toolExecutorInput{
				result:       &appconfig.LoadResult{Config: &cfg, CapabilityReport: capabilitytest.All(t)},
				db:           db,
				artifactRoot: t.TempDir(),
				logger:       slog.New(slog.DiscardHandler),
				wiring:       &toolWiring{},
				events:       turn.NewEventBus(slog.New(slog.DiscardHandler)),
			})
			if err != nil {
				t.Fatalf("newTestToolExecutor: %v", err)
			}
			if _, ok := executor.Broker().Definition("web_search", "v1"); !ok {
				t.Fatal("web_search is not registered")
			}
		})
	}
}

func TestSearXNGToolsConfigReachesALoopbackServerThroughTheExecutorAdapters(t *testing.T) {
	t.Setenv("AURA_TEST_SEARXNG_TOKEN", "searxng-bearer-value")
	var mu sync.Mutex
	var seen *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = r.Clone(r.Context())
		mu.Unlock()
		_, _ = io.WriteString(w, `{"results":[{"title":"Go","url":"https://go.dev","content":"searxng-bearer-value"}]}`)
	}))
	defer server.Close()
	cfg := appconfig.Default()
	cfg.Tools.Workspace = t.TempDir()
	cfg.Tools.WebSearch = search.Config{
		Provider:      search.ProviderSearXNG,
		Endpoint:      server.URL + "/search",
		CredentialRef: secrettest.Ref("env://AURA_TEST_SEARXNG_TOKEN"),
		MaxResults:    5,
	}
	db := sqlitetest.OpenMigrated(t, Schemas()...)
	executor, err := newTestToolExecutor(&toolExecutorInput{
		result:       &appconfig.LoadResult{Config: &cfg, CapabilityReport: capabilitytest.All(t)},
		db:           db,
		artifactRoot: t.TempDir(),
		logger:       slog.New(slog.DiscardHandler),
		wiring:       &toolWiring{},
		events:       turn.NewEventBus(slog.New(slog.DiscardHandler)),
	})
	if err != nil {
		t.Fatalf("newTestToolExecutor: %v", err)
	}
	got, err := executor.Execute(t.Context(), &runtimeadk.BuiltinToolRequest{
		RequestID:    "search-1",
		TurnID:       "turn-1",
		SessionID:    "session-1",
		PrincipalID:  "owner-1",
		ToolName:     "web_search",
		ToolVersion:  "v1",
		Arguments:    json.RawMessage(`{"query":"go"}`),
		Capabilities: []string{"provider-search"},
		Trust:        string(approval.TrustOwnerInput),
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var decoded struct {
		Results []struct {
			Title   string `json:"title"`
			Snippet string `json:"snippet"`
		} `json:"results"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil || len(decoded.Results) != 1 || decoded.Results[0].Title != "Go" || decoded.Results[0].Snippet != "[redacted]" {
		t.Fatalf("output = %s, err = %v", got, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen == nil || seen.URL.Path != "/search" || seen.URL.Query().Get("q") != "go" || seen.Header.Get("Authorization") != "Bearer searxng-bearer-value" {
		t.Fatalf("request = %+v", seen)
	}
}

func TestExecutorRejectsASearchEndpointItCannotParse(t *testing.T) {
	cfg := appconfig.Default()
	cfg.Tools.Workspace = t.TempDir()
	cfg.Tools.WebSearch = search.Config{Provider: search.ProviderSearXNG, Endpoint: "http://localhost:8888/search", MaxResults: 5}
	db := sqlitetest.OpenMigrated(t, Schemas()...)
	if _, err := newTestToolExecutor(&toolExecutorInput{
		result:       &appconfig.LoadResult{Config: &cfg, CapabilityReport: capabilitytest.All(t)},
		db:           db,
		artifactRoot: t.TempDir(),
		logger:       slog.New(slog.DiscardHandler),
		wiring:       &toolWiring{},
		events:       turn.NewEventBus(slog.New(slog.DiscardHandler)),
	}); err == nil {
		t.Fatal("newTestToolExecutor accepted a loopback name endpoint")
	}
}
