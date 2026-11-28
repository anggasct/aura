//go:build durable

package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"google.golang.org/genai"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/workflow"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	auramodel "github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/toolbroker/toolbrokertest"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/platform/usage"
)

const (
	agentStepDefinitionID = "agent-crash-resume"
	agentStepID           = "report"
	agentStepReadTool     = "read_file"
	agentStepToolResult   = "tool-result"
	agentStepFinalText    = "the note says: the first note"
)

var agentStepScript = []agentScriptStep{
	{call: &genai.FunctionCall{Name: agentStepReadTool, Args: map[string]any{"path": "first.txt"}}},
	{text: agentStepFinalText},
}

type gatedStepEvents struct {
	inner  workflow.StepEvents
	ledger *turnLedger
}

func (g *gatedStepEvents) UpsertEvent(ctx context.Context, event *turn.Event) (uint64, bool, error) {
	if g.ledger.life == 1 && event.Kind == turnstore.EventKindADK {
		decoded, err := turnstore.RuntimeEventToADK(event)
		if err != nil {
			return 0, false, err
		}
		if decoded.Content != nil {
			for _, part := range decoded.Content.Parts {
				if part != nil && part.FunctionResponse != nil {
					if err := g.ledger.gate(ctx, agentStepToolResult); err != nil {
						return 0, false, err
					}
				}
			}
		}
	}
	return g.inner.UpsertEvent(ctx, event)
}

func agentStepSpec() *workflow.Spec {
	persona, prompt := scenarioPersona, "Read first.txt and report what it says."
	return &workflow.Spec{
		ID: agentStepDefinitionID, Goal: "Resume an agent step after a killed process", Version: 1, Source: workflow.SourceDefined, RunAs: scenarioPersona,
		Steps: []workflow.StepSpec{{
			ID:       agentStepID,
			Executor: workflow.ExecutorSpec{Kind: workflow.KindAgent, AgentID: &persona, Prompt: prompt, Tools: []string{agentStepReadTool}},
			Timeout:  2 * time.Minute,
		}},
	}
}

func seedAgentStep(t *testing.T, suite *runSuite) {
	t.Helper()
	spec := agentStepSpec()
	if err := mustWorkflowStore(t, suite.db).SaveDefinition(t.Context(), spec); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}
	digest, err := workflow.Digest(spec)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if _, err := suite.db.ExecContext(t.Context(),
		`INSERT INTO workflow_approval (definition_id, definition_version, spec_sha256, kind, approver_id, tainted, approved_at) VALUES (?, ?, ?, 'installed', ?, 0, ?)`,
		spec.ID, spec.Version, digest, principal.OwnerID, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("approve the seeded definition: %v", err)
	}
}

func hostAgentStep(t *testing.T, life *runLife) {
	t.Helper()
	cfg := appconfig.Default()
	persona := auraagent.Persona{ID: scenarioPersona, Description: "reads the note", Instructions: "read the note and report", Tools: []string{agentStepReadTool}}
	registry, err := auraagent.Build([]auraagent.Persona{persona}, []string{agentStepReadTool}, appconfig.BuiltinToolDefinitions(), []string{"primary"}, "en")
	if err != nil {
		t.Fatalf("agent registry: %v", err)
	}
	broker := toolbrokertest.NewBroker(t, &toolbroker.Options{HostCapabilities: capabilitytest.Host(t)})
	registerStubBuiltin(t, broker, agentStepReadTool, false, life.ledger.adapter(stubTool{name: agentStepReadTool}))
	builtin, err := runtimeadk.NewBrokerExecutor(broker, capabilitytest.Host(t))
	if err != nil {
		t.Fatalf("NewBrokerExecutor: %v", err)
	}
	prices := usage.NewPriceRegistry()
	if err := prices.Put(&usage.Price{
		ModelDefinitionID: "primary", Currency: cfg.Usage.Currency, MicrosPerInputToken: 1, MicrosPerOutputToken: 1,
		MaxReservationRate: 100, EffectiveFrom: time.Unix(0, 0).UTC(),
	}); err != nil {
		t.Fatalf("put the price: %v", err)
	}
	ledger, err := usage.NewTurnLedger(cfg.Usage, cfg.Access, life.db, prices, nil)
	if err != nil {
		t.Fatalf("NewTurnLedger: %v", err)
	}
	script := &agentScriptModel{steps: agentStepScript, arrive: func(request *agentModelRequest) (int, error) {
		encoded, err := json.Marshal(request)
		if err != nil {
			return 0, err
		}
		return life.ledger.record(modelCalls, string(encoded))
	}}
	sessions := turnstore.NewSessionService(life.db)
	executor, err := newAgentStepExecutor(&agentStepExecutorInput{
		models: auramodel.Set{"primary": script}, modelName: "primary", sessions: sessions, builtin: builtin,
		ledger: ledger, registry: registry, routeModel: func(string) (string, error) { return "primary", nil },
		logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("newAgentStepExecutor: %v", err)
	}
	deps := &workflow.ValidationDeps{
		KnownTools: []string{agentStepReadTool}, Agents: registry, CheckToolCall: acceptToolCall,
		StepAllowed: stepCeiling(registry, newStepCatalog(broker.Definitions())),
	}
	runner, err := workflow.NewAgentTurnRunner(&workflow.AgentTurnRunnerDeps{
		Turns: executor, Events: &gatedStepEvents{inner: turnstore.NewEventStore(life.db), ledger: life.ledger},
		Sessions: sessions, Tools: broker, Runs: life.workflows, Validation: deps, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("NewAgentTurnRunner: %v", err)
	}
	interpreter := mustWorkflowInterpreter(t, life.workflows, life.runtime, &workflow.Options{
		MaxConcurrentSteps: cfg.Workflows.MaxConcurrentSteps,
		Logger:             slog.New(slog.DiscardHandler),
		AgentSteps:         runner,
	})
	if err := interpreter.LoadAll(t.Context(), []*workflow.Spec{agentStepSpec()}, deps); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	life.interpreter = interpreter
}

func startAgentStepRun(t *testing.T, life *runLife) {
	t.Helper()
	summary, err := life.interpreter.StartConfigured(t.Context(), &workflow.ConfiguredStart{DefinitionID: agentStepDefinitionID, Input: &workflow.RunInput{Objective: "survive the kill"}, PrincipalID: principal.OwnerID, Class: principal.ClassOwner})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	life.plan.Run, life.plan.Key = summary.ID, summary.DurableKey
}
