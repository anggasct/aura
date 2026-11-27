package runtimeadk

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/storage"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/toolbroker/toolbrokertest"
	auratools "github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

func TestNewBrokerExecutorRejectsNilBroker(t *testing.T) {
	executor, err := NewBrokerExecutor(nil, nil)
	if executor != nil || !fault.HasCode(err, turn.ErrorCodeInvalidArgument) {
		t.Fatalf("NewBrokerExecutor(nil) = %v, %v; want %s", executor, err, turn.ErrorCodeInvalidArgument)
	}
}

func TestZeroBrokerExecutorFailsClosed(t *testing.T) {
	for _, exec := range []*BrokerExecutor{nil, {}} {
		if defs := exec.Definitions(); defs != nil {
			t.Fatalf("Definitions() = %v, want nil", defs)
		}
		if _, err := exec.Evaluate(t.Context(), &approval.ToolRequest{}); !fault.HasCode(err, approval.ErrorCodeInvalidArgument) {
			t.Fatalf("Evaluate() err = %v, want %s", err, approval.ErrorCodeInvalidArgument)
		}
		if _, err := exec.Execute(t.Context(), &BuiltinToolRequest{}); !fault.HasCode(err, turn.ErrorCodeInvalidArgument) {
			t.Fatalf("Execute() err = %v, want %s", err, turn.ErrorCodeInvalidArgument)
		}
	}
}

func TestDefinitionsCarryTheApprovedPluginDescription(t *testing.T) {
	broker, err := toolbroker.New(&toolbroker.Options{})
	if err != nil {
		t.Fatalf("toolbroker.New: %v", err)
	}
	executor, err := NewBrokerExecutor(broker, nil)
	if err != nil {
		t.Fatalf("NewBrokerExecutor: %v", err)
	}
	definition := &auratools.Definition{
		Name: "plugin_weather_forecast", Version: "v1", Plugin: &auratools.PluginRef{ID: "weather", Description: "Reviewed forecast text."},
		Schema: json.RawMessage(`{"type":"object"}`), RequiresApproval: true, Effectful: true,
	}
	rule := approval.Rule{ToolName: definition.Name, ToolVersion: "v1", RequiresApproval: true, AllowedTrust: []approval.TrustLabel{approval.TrustOwnerInput}}
	adapter := func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
		return toolbroker.ToolResult{}, nil
	}
	if err := broker.RegisterTool(definition, adapter, &rule); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
	for _, listed := range executor.Definitions() {
		if listed.Name != definition.Name {
			continue
		}
		if listed.Plugin != "weather" || listed.Server != "" || listed.Description != "Reviewed forecast text." {
			t.Fatalf("listed = %+v", listed)
		}
		return
	}
	t.Fatal("plugin tool is not listed")
}

func TestBrokerExecutorPropagatesChannelAndFiltersCapabilities(t *testing.T) {
	broker, err := toolbroker.New(&toolbroker.Options{HostCapabilities: []string{"workspace-read"}})
	if err != nil {
		t.Fatalf("toolbroker.New: %v", err)
	}
	noop := func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
		return toolbroker.ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
	}
	defs := []auratools.Definition{
		{Name: "email_send", Version: "v1", Channel: "email", OptIn: true, Effectful: true, Schema: json.RawMessage(`{"type":"object"}`)},
		{Name: "read_file", Version: "v1", RequiredCapabilities: []string{"workspace-read"}, Schema: json.RawMessage(`{"type":"object"}`)},
		{Name: "write_file", Version: "v1", RequiredCapabilities: []string{"workspace-write"}, RequiresApproval: true, Effectful: true, Schema: json.RawMessage(`{"type":"object"}`)},
	}
	for i := range defs {
		rule := toolbroker.DefaultRule(&defs[i])
		if err := broker.RegisterTool(&defs[i], noop, &rule); err != nil {
			t.Fatalf("RegisterTool(%s): %v", defs[i].Name, err)
		}
	}
	executor, err := NewBrokerExecutor(broker, []string{"workspace-read"})
	if err != nil {
		t.Fatalf("NewBrokerExecutor: %v", err)
	}
	listed := executor.Definitions()
	names := make([]string, 0, len(listed))
	var emailChannel string
	for i := range listed {
		names = append(names, listed[i].Name)
		if listed[i].Name == "email_send" {
			emailChannel = listed[i].Channel
		}
	}
	if !slices.Equal(names, []string{"email_send", "read_file"}) {
		t.Fatalf("offered = %v, want [email_send read_file]", names)
	}
	if emailChannel != "email" {
		t.Fatalf("email_send channel = %q, want email", emailChannel)
	}
}

func TestTurnToolCallsLetTheJournalAllocateTheSequence(t *testing.T) {
	ctx := t.Context()
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), effect.Schema(), storage.Schema())
	const session = "session-allocate"
	if err := turnstore.NewSessionService(db).Create(ctx, &turn.Session{ID: session, OwnerID: principal.OwnerID}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	journal, err := effect.NewJournal(db, effect.Options{Turns: turnstore.NewTurnStore(db)})
	if err != nil {
		t.Fatalf("effect.NewJournal: %v", err)
	}
	effects, err := effect.NewExecutor(journal)
	if err != nil {
		t.Fatalf("effect.NewExecutor: %v", err)
	}
	broker, err := toolbroker.New(&toolbroker.Options{Effects: effects})
	if err != nil {
		t.Fatalf("toolbroker.New: %v", err)
	}
	executor, err := NewBrokerExecutor(broker, nil)
	if err != nil {
		t.Fatalf("NewBrokerExecutor: %v", err)
	}
	var seen []toolbroker.ToolRequest
	toolbrokertest.Register(t, broker, toolbrokertest.Stub{
		Name: "stub_effect", Effectful: true,
		Run: func(_ context.Context, req *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
			seen = append(seen, *req)
			return toolbroker.ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
		},
	})
	request := &BuiltinToolRequest{
		RequestID: "call-allocate", TurnID: "turn-allocate", SessionID: session,
		PrincipalID: principal.OwnerID, PrincipalClass: principal.ClassOwner,
		ToolName: "stub_effect", ToolVersion: "v1", Arguments: json.RawMessage(`{"n":1}`),
		Trust: string(approval.TrustDerivedUntrusted), Deadline: time.Now().Add(time.Minute),
		IdempotencyKey: "adk/" + session + "/turn-allocate/call-allocate", EventInvocation: "inv-allocate",
		EventBranch: "main", EventAuthor: "agent",
	}
	events := turnstore.NewEventStore(db)
	for sequence := uint64(1); sequence <= 2; sequence++ {
		if err := events.Append(ctx, &turn.Event{
			ID: "other-" + string(rune('0'+sequence)), SessionID: session, Sequence: sequence, TurnID: "turn-other",
			Kind: turn.EventKindTurnAccepted, SchemaVersion: 1, Payload: json.RawMessage(`{}`), CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("append the other turn's event %d: %v", sequence, err)
		}
	}
	if _, err := executor.Execute(ctx, request); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(seen) != 1 || !seen[0].AllocateEventSequence || seen[0].EventSequence != 0 {
		t.Fatalf("broker requests = %+v, want one that lets the journal allocate its sequence", seen)
	}
	var sequence uint64
	if err := db.QueryRowContext(ctx, `SELECT sequence FROM runtime_event WHERE session_id = ? AND kind = ?`, session, turn.EventKindToolRequested).Scan(&sequence); err != nil {
		t.Fatalf("read the tool.requested event: %v", err)
	}
	if sequence != 3 {
		t.Fatalf("tool.requested sequence = %d, want 3, the session's next free sequence", sequence)
	}
}

func TestExecutorForwardsTheTaintFlagToTheBroker(t *testing.T) {
	decisions := 0
	broker, err := toolbroker.New(&toolbroker.Options{
		HostCapabilities: []string{"provider-search"},
		ApprovalDecider: func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) {
			decisions++
			return false, nil
		},
		ApprovalReachable: func(sessionID string) bool { return sessionID == "reachable" },
	})
	if err != nil {
		t.Fatalf("toolbroker.New: %v", err)
	}
	searchDef := &auratools.Definition{
		Name: "web_search", Version: "v1", RequiredCapabilities: []string{"provider-search"}, ExternalSink: true,
		Schema: json.RawMessage(`{"type":"object"}`),
		Validator: func(raw json.RawMessage) (json.RawMessage, error) {
			return raw, nil
		},
	}
	searchRule := toolbroker.DefaultRule(searchDef)
	if err := broker.RegisterTool(searchDef, func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
		return toolbroker.ToolResult{Output: json.RawMessage(`{"results":[]}`)}, nil
	}, &searchRule); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
	executor, err := NewBrokerExecutor(broker, []string{"provider-search"})
	if err != nil {
		t.Fatalf("NewBrokerExecutor: %v", err)
	}
	request := func(session string, tainted bool) *BuiltinToolRequest {
		return &BuiltinToolRequest{
			RequestID: "call-1", TurnID: "turn-1", SessionID: session, PrincipalID: "owner-1",
			ToolName: "web_search", ToolVersion: "v1", Arguments: json.RawMessage(`{"query":"x"}`),
			Capabilities: []string{"provider-search"}, Trust: "derived_untrusted", Deadline: time.Now().Add(time.Minute),
			IdempotencyKey: "k", Tainted: tainted, Knowledge: []string{"faq"},
		}
	}
	_, err = executor.Execute(t.Context(), request("unreachable", true))
	if class, _ := fault.CodeOf[toolbroker.ResultClass](err); class != toolbroker.ResultUntrustedInputNoApprover || decisions != 0 {
		t.Fatalf("tainted on an unreachable session: %v, decisions %d", err, decisions)
	}
	_, err = executor.Execute(t.Context(), request("reachable", true))
	if class, _ := fault.CodeOf[toolbroker.ResultClass](err); class != toolbroker.ResultApprovalRequired || decisions != 1 {
		t.Fatalf("tainted on a reachable session: %v, decisions %d, want a rejected prompt", err, decisions)
	}
	_, err = executor.Execute(t.Context(), request("unreachable", false))
	if class, _ := fault.CodeOf[toolbroker.ResultClass](err); class == toolbroker.ResultUntrustedInputNoApprover || decisions != 1 {
		t.Fatalf("untainted call: %v, decisions %d, want no taint denial and no prompt", err, decisions)
	}
}
