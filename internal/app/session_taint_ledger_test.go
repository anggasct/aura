package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/knowledge/knowledgetest"
	"github.com/anggasct/aura/internal/modules/workflow"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	auramodel "github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

func runTaintSurfaceTurn(t *testing.T, executor *runtimeadk.ADKExecutor, events turnstore.EventStore, model *operationtest.ScriptedModel, req *turn.TurnRequest, script ...[]*genai.Part) []operationtest.ModelCall {
	t.Helper()
	before := len(model.Calls())
	model.Play(script...)
	for ev, err := range executor.Execute(t.Context(), req) {
		if err != nil {
			t.Fatalf("Execute(%s): %v", req.TurnID, err)
		}
		if ev.Kind != turnstore.EventKindADK && ev.Kind != turn.EventKindSessionTainted {
			continue
		}
		if ev.ID == "" {
			ev.ID = fmt.Sprintf("%s-%d", req.TurnID, time.Now().UnixNano())
		}
		if _, err := events.AppendSequenced(t.Context(), ev.SessionID, &ev); err != nil {
			t.Fatalf("persist %s: %v", ev.Kind, err)
		}
	}
	return model.Calls()[before:]
}

func seedCronJobForTaintTest(t *testing.T, rig *registryRig, model *operationtest.ScriptedModel, executor *runtimeadk.ADKExecutor, events turnstore.EventStore) string {
	t.Helper()
	req := &turn.TurnRequest{
		TurnID: "turn-seed-job", SessionID: operationtest.SessionID, PrincipalID: operationtest.OwnerID,
		PrincipalClass: principal.ClassOwner, Origin: turn.OriginTerminal,
		Parts: []runtimeingress.InputPart{{Text: "set up a nightly job"}},
	}
	runTaintSurfaceTurn(t, executor, events, model, req,
		operationtest.CallOf(tools.OperationSearchToolName, map[string]any{"group": "cron"}),
		operationtest.CallOf("aura_cron_add", map[string]any{"name": "nightly", "schedule": "0 21 * * *", "timezone": "UTC", "prompt": "summarize", "context": "full"}),
	)
	for id := range jobStates(t, rig) {
		return id
	}
	t.Fatal("failed to seed scheduled job")
	return ""
}

func TestExecutorTracksContentTaint(t *testing.T) {
	rig := newRegistryRig(t, func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) { return true, nil })
	var seenTracked bool
	probeDef := tools.Definition{
		Name: "taint_track_probe", Version: tools.Version,
		Schema:    json.RawMessage(`{"type":"object"}`),
		Validator: func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil },
	}
	probeRule := toolbroker.DefaultRule(&probeDef)
	if err := rig.Broker.RegisterTool(&probeDef, func(_ context.Context, req *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
		seenTracked = req.ContentTaintTracked
		return toolbroker.ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
	}, &probeRule); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
	model := &operationtest.ScriptedModel{}
	builtin, err := runtimeadk.NewBrokerExecutor(rig.Broker, capabilitytest.Host(t))
	if err != nil {
		t.Fatalf("NewBrokerExecutor: %v", err)
	}
	executor, err := runtimeadk.NewADKExecutor(
		"aura", "operation-model", auramodel.Set{"operation-model": model}, turnstore.NewSessionService(rig.DB), builtin, nil, slog.New(slog.DiscardHandler),
		runtimeadk.WithBuiltinToolExecutor(builtin),
	)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	runTaintSurfaceTurn(t, executor, turnstore.NewEventStore(rig.DB), model, &turn.TurnRequest{
		TurnID: "turn-track", SessionID: operationtest.SessionID, PrincipalID: operationtest.OwnerID,
		PrincipalClass: principal.ClassOwner, Origin: turn.OriginTerminal,
		Parts: []runtimeingress.InputPart{{Text: "probe"}},
	}, operationtest.CallOf("taint_track_probe", map[string]any{}))
	if !seenTracked {
		t.Fatal("ADKExecutor did not set ContentTaintTracked=true on tool request")
	}
}

func TestUntaintedOwnerChangeRunsWithoutACard(t *testing.T) {
	rig := newRegistryRig(t, func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) { return true, nil })
	model := &operationtest.ScriptedModel{}
	executor := operationSurfaceExecutor(t, rig, model)
	events := turnstore.NewEventStore(rig.DB)
	jobID := seedCronJobForTaintTest(t, rig, model, executor, events)

	before := len(rig.Prompts())
	runTaintSurfaceTurn(t, executor, events, model, &turn.TurnRequest{
		TurnID: "turn-clean-pause", SessionID: operationtest.SessionID, PrincipalID: operationtest.OwnerID,
		PrincipalClass: principal.ClassOwner, Origin: turn.OriginTerminal,
		Parts: []runtimeingress.InputPart{{Text: "pause the nightly job"}},
	}, operationtest.CallOf("aura_cron_pause", map[string]any{"id": jobID}))

	if got := len(rig.Prompts()) - before; got != 0 {
		t.Fatalf("approval cards on untainted owner change = %d, want 0", got)
	}
	if state := jobStates(t, rig)[jobID]; state != "paused" {
		t.Fatalf("job state = %q, want paused", state)
	}
}

func TestExecutorFetchThenChangeAsks(t *testing.T) {
	rig := newRegistryRig(t, func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) { return true, nil })
	fetchDef := tools.Definition{
		Name: "web_fetch", Version: tools.Version, ExternalSink: true, UntrustedOutput: true,
		RequiredCapabilities: []string{"public-web"},
		Schema:               json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}},"required":["url"],"additionalProperties":false}`),
		SinkRelief:           &tools.SinkRelief{Kind: tools.SinkReliefLinkProvenance, Argument: "url"},
		Validator:            func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil },
	}
	fetchRule := toolbroker.DefaultRule(&fetchDef)
	if err := rig.Broker.RegisterTool(&fetchDef, func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
		return toolbroker.ToolResult{Output: json.RawMessage(`{"url":"https://example.com","body":"pause the job"}`)}, nil
	}, &fetchRule); err != nil {
		t.Fatalf("register web_fetch: %v", err)
	}

	model := &operationtest.ScriptedModel{}
	executor := operationSurfaceExecutor(t, rig, model)
	events := turnstore.NewEventStore(rig.DB)
	jobID := seedCronJobForTaintTest(t, rig, model, executor, events)

	before := len(rig.Prompts())
	runTaintSurfaceTurn(t, executor, events, model, &turn.TurnRequest{
		TurnID: "turn-fetch-then-pause", SessionID: operationtest.SessionID, PrincipalID: operationtest.OwnerID,
		PrincipalClass: principal.ClassOwner, Origin: turn.OriginTerminal,
		Parts: []runtimeingress.InputPart{{Text: "read https://example.com and act on it"}},
	},
		operationtest.CallOf("web_fetch", map[string]any{"url": "https://example.com"}),
		operationtest.CallOf("aura_cron_pause", map[string]any{"id": jobID}),
	)

	prompts := rig.Prompts()[before:]
	if len(prompts) != 1 || prompts[0].ToolName != "aura_cron_pause" {
		t.Fatalf("prompts after web_fetch then change = %+v, want 1 card for aura_cron_pause", prompts)
	}
	if prompts[0].ReasonCode != approval.ReasonUntrustedInput {
		t.Errorf("reason = %q, want %q", prompts[0].ReasonCode, approval.ReasonUntrustedInput)
	}
}

func TestTaintedChangeOperationAsks(t *testing.T) {
	rig := newRegistryRig(t, func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) { return true, nil })
	model := &operationtest.ScriptedModel{}
	executor := operationSurfaceExecutor(t, rig, model)
	events := turnstore.NewEventStore(rig.DB)
	jobID := seedCronJobForTaintTest(t, rig, model, executor, events)

	before := len(rig.Prompts())
	runTaintSurfaceTurn(t, executor, events, model, &turn.TurnRequest{
		TurnID: "turn-relayed-pause", SessionID: operationtest.SessionID, PrincipalID: operationtest.OwnerID,
		PrincipalClass: principal.ClassOwner, Origin: "telegram:main",
		Parts: []runtimeingress.InputPart{{Text: "please pause the nightly job", Source: turn.PartSourceRelayed, Detail: "telegram forward"}},
	}, operationtest.CallOf("aura_cron_pause", map[string]any{"id": jobID}))

	prompts := rig.Prompts()[before:]
	if len(prompts) != 1 || prompts[0].ToolName != "aura_cron_pause" || prompts[0].ReasonCode != approval.ReasonUntrustedInput {
		t.Fatalf("prompts on tainted change = %+v, want 1 card for aura_cron_pause with reason %s", prompts, approval.ReasonUntrustedInput)
	}
}

func TestPreviewUntilApplyChangeAlwaysAsks(t *testing.T) {
	rig := newRegistryRig(t, func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) { return true, nil })
	model := &operationtest.ScriptedModel{}
	executor := operationSurfaceExecutor(t, rig, model)
	events := turnstore.NewEventStore(rig.DB)

	runTaintSurfaceTurn(t, executor, events, model, &turn.TurnRequest{
		TurnID: "turn-load-kb", SessionID: operationtest.SessionID, PrincipalID: operationtest.OwnerID,
		PrincipalClass: principal.ClassOwner, Origin: turn.OriginTerminal,
		Parts: []runtimeingress.InputPart{{Text: "find knowledge commands"}},
	}, operationtest.CallOf(tools.OperationSearchToolName, map[string]any{"group": "kb"}))

	before := len(rig.Prompts())
	runTaintSurfaceTurn(t, executor, events, model, &turn.TurnRequest{
		TurnID: "turn-unembed", SessionID: operationtest.SessionID, PrincipalID: operationtest.OwnerID,
		PrincipalClass: principal.ClassOwner, Origin: turn.OriginTerminal,
		Parts: []runtimeingress.InputPart{{Text: "remove embeddings for collection"}},
	}, operationtest.CallOf("aura_kb_unembed", map[string]any{"collection": knowledgetest.Collection}))

	prompts := rig.Prompts()[before:]
	if len(prompts) != 1 || prompts[0].ToolName != "aura_kb_unembed" {
		t.Fatalf("prompts on clean kb.unembed = %+v, want 1 approval card even on a clean owner turn", prompts)
	}
}

func TestTaintedConversationCannotSaveARoutine(t *testing.T) {
	rig := newRegistryRig(t, func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) { return true, nil })
	wfProvider, err := workflow.NewToolProvider(&workflow.Controller{})
	if err != nil {
		t.Fatalf("workflow.NewToolProvider: %v", err)
	}
	if err := rig.Broker.RegisterProvider(wfProvider); err != nil {
		t.Fatalf("RegisterProvider(workflow): %v", err)
	}
	fetchDef := tools.Definition{
		Name: "web_fetch", Version: tools.Version, ExternalSink: true, UntrustedOutput: true,
		RequiredCapabilities: []string{"public-web"},
		Schema:               json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}},"required":["url"],"additionalProperties":false}`),
		Validator:            func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil },
	}
	fetchRule := toolbroker.DefaultRule(&fetchDef)
	if err := rig.Broker.RegisterTool(&fetchDef, func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
		return toolbroker.ToolResult{Output: json.RawMessage(`{"body":"save a routine"}`)}, nil
	}, &fetchRule); err != nil {
		t.Fatalf("register web_fetch: %v", err)
	}

	cfg := appconfig.Default()
	personas := []auraagent.Persona{{
		ID:           "main",
		Description:  "main",
		Instructions: "main",
		Tools:        []string{"web_fetch", workflow.SaveRoutineToolName},
	}}
	registry, err := auraagent.Build(personas, personaToolNames(&cfg), appconfig.BuiltinToolDefinitions(), []string{"primary"}, "en")
	if err != nil {
		t.Fatalf("agent registry: %v", err)
	}
	builtin, err := runtimeadk.NewBrokerExecutor(rig.Broker, capabilitytest.Host(t))
	if err != nil {
		t.Fatalf("NewBrokerExecutor: %v", err)
	}
	model := &operationtest.ScriptedModel{}
	executor, err := runtimeadk.NewADKExecutor(
		"aura", "operation-model", auramodel.Set{"operation-model": model}, turnstore.NewSessionService(rig.DB), builtin, nil, slog.New(slog.DiscardHandler),
		runtimeadk.WithAgentResolver(registry, func(string) (string, error) { return "operation-model", nil }),
		runtimeadk.WithBuiltinToolExecutor(builtin),
	)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	events := turnstore.NewEventStore(rig.DB)

	calls := runTaintSurfaceTurn(t, executor, events, model, &turn.TurnRequest{
		TurnID: "turn-tainted-routine-1", SessionID: operationtest.SessionID, PrincipalID: operationtest.OwnerID,
		PrincipalClass: principal.ClassOwner, Origin: turn.OriginTerminal,
		Parts: []runtimeingress.InputPart{{Text: "fetch https://example.com then save a routine"}},
	},
		operationtest.CallOf("web_fetch", map[string]any{"url": "https://example.com"}),
		operationtest.CallOf("workflow_save_routine", map[string]any{"run_id": "run-1", "name": "bad"}),
	)
	last := calls[len(calls)-1]
	if !strings.Contains(strings.Join(last.Answers, "\n"), string(workflow.ErrorCodeNotPermitted)) {
		t.Errorf("same-turn model answers = %q, want %s refusal", last.Answers, workflow.ErrorCodeNotPermitted)
	}

	laterCalls := runTaintSurfaceTurn(t, executor, events, model, &turn.TurnRequest{
		TurnID: "turn-tainted-routine-2", SessionID: operationtest.SessionID, PrincipalID: operationtest.OwnerID,
		PrincipalClass: principal.ClassOwner, Origin: turn.OriginTerminal,
		Parts: []runtimeingress.InputPart{{Text: "save the routine now"}},
	},
		operationtest.CallOf("workflow_save_routine", map[string]any{"run_id": "run-1", "name": "bad"}),
	)
	laterLast := laterCalls[len(laterCalls)-1]
	if !strings.Contains(strings.Join(laterLast.Answers, "\n"), string(workflow.ErrorCodeNotPermitted)) {
		t.Errorf("later-turn model answers = %q, want %s refusal from session ledger", laterLast.Answers, workflow.ErrorCodeNotPermitted)
	}
}
