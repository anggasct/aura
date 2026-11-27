package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/genai"

	appconfig "github.com/anggasct/aura/internal/app/config"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/fault"
	auramodel "github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/toolbroker/toolbrokertest"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/platform/usage"
)

type recordingBuiltins struct {
	definitions []runtimeadk.BuiltinToolDefinition
	forwarded   []runtimeadk.BuiltinToolRequest
}

func (r *recordingBuiltins) Definitions() []runtimeadk.BuiltinToolDefinition {
	return r.definitions
}

func (r *recordingBuiltins) Execute(_ context.Context, request *runtimeadk.BuiltinToolRequest) (json.RawMessage, error) {
	r.forwarded = append(r.forwarded, *request)
	return json.RawMessage(`{}`), nil
}

func definitionsNamed(names ...string) []runtimeadk.BuiltinToolDefinition {
	definitions := make([]runtimeadk.BuiltinToolDefinition, 0, len(names))
	for _, name := range names {
		definitions = append(definitions, runtimeadk.BuiltinToolDefinition{Name: name, Version: "v1"})
	}
	return definitions
}

func TestAgentStepBuiltinsExposeOnlyReadOnlyTools(t *testing.T) {
	everything := []string{
		"exec", "code_execute", "read_file", "write_file", "list_dir", "web_fetch", "web_search", "kb_search",
		"email_send", "handoff_to_owner", "github_create_pr", "mcp_files_read", "workflow_start", "workflow_propose",
	}
	tests := []struct {
		name       string
		registered []string
		want       []string
	}{
		{"the full surface catalog", everything, []string{"read_file", "list_dir", "web_fetch", "web_search"}},
		{"a host without the web tools", []string{"read_file", "write_file", "list_dir", "exec"}, []string{"read_file", "list_dir"}},
		{"a host with only write tools", []string{"write_file", "exec", "workflow_start"}, []string{}},
		{"an empty catalog", nil, []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inner := &recordingBuiltins{definitions: definitionsNamed(tc.registered...)}
			wrapper, err := newReadOnlyAgentStepTools(inner)
			if err != nil {
				t.Fatalf("newReadOnlyAgentStepTools: %v", err)
			}
			var offered []string
			for _, definition := range wrapper.Definitions() {
				offered = append(offered, definition.Name)
			}
			if offered == nil {
				offered = []string{}
			}
			slices.Sort(offered)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(offered, want) {
				t.Fatalf("Definitions() = %v, want %v", offered, want)
			}
		})
	}

	t.Run("Execute refuses any other name before the inner executor", func(t *testing.T) {
		for _, name := range everything {
			if slices.Contains(agentStepToolNames, name) {
				continue
			}
			inner := &recordingBuiltins{definitions: definitionsNamed(everything...)}
			wrapper, err := newReadOnlyAgentStepTools(inner)
			if err != nil {
				t.Fatalf("newReadOnlyAgentStepTools: %v", err)
			}
			_, err = wrapper.Execute(t.Context(), &runtimeadk.BuiltinToolRequest{ToolName: name, ToolVersion: "v1"})
			if !fault.HasCode(err, toolbroker.ResultPolicyDenied) {
				t.Errorf("Execute(%s) = %v, want %s", name, err, toolbroker.ResultPolicyDenied)
			}
			if len(inner.forwarded) != 0 {
				t.Errorf("Execute(%s) reached the inner executor", name)
			}
		}
	})

	t.Run("Execute marks every forwarded request unattended", func(t *testing.T) {
		for _, name := range agentStepToolNames {
			inner := &recordingBuiltins{definitions: definitionsNamed(agentStepToolNames...)}
			wrapper, err := newReadOnlyAgentStepTools(inner)
			if err != nil {
				t.Fatalf("newReadOnlyAgentStepTools: %v", err)
			}
			request := &runtimeadk.BuiltinToolRequest{ToolName: name, ToolVersion: "v1", Unattended: false}
			if _, err := wrapper.Execute(t.Context(), request); err != nil {
				t.Fatalf("Execute(%s): %v", name, err)
			}
			if len(inner.forwarded) != 1 || !inner.forwarded[0].Unattended {
				t.Errorf("Execute(%s) forwarded %+v, want one unattended request", name, inner.forwarded)
			}
			if request.Unattended {
				t.Errorf("Execute(%s) changed the caller's request", name)
			}
		}
	})

	t.Run("a nil executor or request is refused", func(t *testing.T) {
		if _, err := newReadOnlyAgentStepTools(nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
			t.Fatalf("newReadOnlyAgentStepTools(nil) = %v, want %s", err, ErrorCodeInvalidArgument)
		}
		wrapper, err := newReadOnlyAgentStepTools(&recordingBuiltins{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := wrapper.Execute(t.Context(), nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
			t.Fatalf("Execute(nil) = %v, want %s", err, ErrorCodeInvalidArgument)
		}
	})
}

func registerStubBuiltin(t testing.TB, broker *toolbroker.Broker, name string, requiresApproval bool, adapter toolbroker.Adapter) {
	t.Helper()
	definition := &tools.Definition{
		Name: name, Version: tools.Version, Schema: json.RawMessage(`{"type":"object"}`),
		Validator: func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil },
	}
	rule := &approval.Rule{ToolName: name, ToolVersion: tools.Version, RequiresApproval: requiresApproval}
	if err := broker.RegisterTool(definition, adapter, rule); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
}

type stepExecutorRig struct {
	input    *agentStepExecutorInput
	model    *agentModelLog
	adapters atomic.Int64
	deciders atomic.Int64
}

func newStepExecutorRig(t *testing.T, stubs map[string]bool, steps []agentScriptStep) *stepExecutorRig {
	t.Helper()
	dir := t.TempDir()
	sqlitetest.Seed(t, filepath.Join(dir, "aura.db"), Schemas()...)
	db, err := sqlite.Open(t.Context(), filepath.Join(dir, "aura.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := appconfig.Default()
	registry, err := auraagent.Build(nil, definitionNames(), appconfig.BuiltinToolDefinitions(), []string{"primary"}, "en")
	if err != nil {
		t.Fatalf("agent registry: %v", err)
	}
	prices := usage.NewPriceRegistry()
	if err := prices.Put(&usage.Price{
		ModelDefinitionID: "primary", Currency: cfg.Usage.Currency, MicrosPerInputToken: 1, MicrosPerOutputToken: 1,
		MaxReservationRate: 100, EffectiveFrom: time.Unix(0, 0).UTC(),
	}); err != nil {
		t.Fatalf("put the price: %v", err)
	}
	ledger, err := usage.NewTurnLedger(cfg.Usage, cfg.Access, db, prices, nil)
	if err != nil {
		t.Fatalf("NewTurnLedger: %v", err)
	}
	rig := &stepExecutorRig{model: &agentModelLog{}}
	broker := toolbrokertest.NewBroker(t, &toolbroker.Options{
		HostCapabilities: capabilitytest.Host(t),
		ApprovalDecider: func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) {
			rig.deciders.Add(1)
			return true, nil
		},
	})
	for name, requiresApproval := range stubs {
		registerStubBuiltin(t, broker, name, requiresApproval, func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
			rig.adapters.Add(1)
			return toolbroker.ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
		})
	}
	builtin, err := runtimeadk.NewBrokerExecutor(broker, capabilitytest.Host(t))
	if err != nil {
		t.Fatalf("NewBrokerExecutor: %v", err)
	}
	script := &agentScriptModel{steps: steps, arrive: rig.model.arrive}
	rig.input = &agentStepExecutorInput{
		models: auramodel.Set{"primary": script}, modelName: "primary", sessions: turnstore.NewSessionService(db), builtin: builtin,
		ledger: ledger, registry: registry, routeModel: func(string) (string, error) { return "primary", nil },
		logger: slog.New(slog.DiscardHandler),
	}
	createStepSession(t, db, "step-session-1")
	return rig
}

func createStepSession(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	now := time.Now().UTC()
	if err := turnstore.NewSessionService(db).Create(t.Context(), &turn.Session{ID: id, OwnerID: principal.OwnerID, AgentID: "researcher", Metadata: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
}

func (r *stepExecutorRig) run(t *testing.T, tainted bool, allowed []string) {
	t.Helper()
	executor, err := newAgentStepExecutor(r.input)
	if err != nil {
		t.Fatalf("newAgentStepExecutor: %v", err)
	}
	req := &turn.TurnRequest{
		TurnID: "step-session-1", SessionID: "step-session-1", IdempotencyKey: "step-session-1",
		PrincipalID: principal.OwnerID, PrincipalClass: principal.ClassOwner, Origin: turn.OriginWorkflow,
		Parts: []runtimeingress.InputPart{{Text: "go"}}, Deadline: time.Now().Add(time.Minute),
		AgentID: "researcher", InputTainted: tainted, ToolRestriction: &turn.ToolRestriction{Allowed: allowed},
	}
	for _, err := range executor.Execute(t.Context(), req) {
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
}

func TestAgentStepExecutorRefusesACallThatNeedsApprovalWithoutAsking(t *testing.T) {
	tests := []struct {
		name    string
		tainted bool
		want    toolbroker.ResultClass
	}{
		{"an untainted call", false, toolbroker.ResultApprovalRequired},
		{"a tainted call", true, toolbroker.ResultUntrustedInputNoApprover},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rig := newStepExecutorRig(t, map[string]bool{"web_search": true, "write_file": false}, []agentScriptStep{
				{call: &genai.FunctionCall{Name: "web_search", Args: map[string]any{"query": "x"}}},
				{text: "answered without the tool"},
			})
			rig.run(t, tc.tainted, []string{"web_search"})
			requests := rig.model.seen()
			if len(requests) != 2 {
				t.Fatalf("model calls = %d, want 2: the refusal goes back to the model and the step continues", len(requests))
			}
			if !slices.Equal(requests[0].Tools, []string{"web_search"}) {
				t.Errorf("tools offered = %v, want exactly [web_search]", requests[0].Tools)
			}
			if len(requests[1].Results) != 1 || !strings.Contains(requests[1].Results[0], string(tc.want)) {
				t.Errorf("the second model request carries %v, want a web_search result holding %s", requests[1].Results, tc.want)
			}
			if got := rig.deciders.Load(); got != 0 {
				t.Errorf("the approval decider was asked %d times, want 0", got)
			}
			if got := rig.adapters.Load(); got != 0 {
				t.Errorf("the tool adapter ran %d times, want 0", got)
			}
		})
	}
}

func TestAgentStepExecutorOffersNoToolWhenTheHostRegistersNoReadOnlyTool(t *testing.T) {
	rig := newStepExecutorRig(t, map[string]bool{"write_file": false, "exec": false}, []agentScriptStep{{text: "thought it through"}})
	rig.run(t, false, []string{"web_search"})
	requests := rig.model.seen()
	if len(requests) != 1 || len(requests[0].Tools) != 0 {
		t.Fatalf("model requests = %+v, want one request that offers no tool", requests)
	}
}
