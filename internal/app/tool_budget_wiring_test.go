package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/genai"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/workflow"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/durable/durabletest"
	auramodel "github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/toolbroker/toolbrokertest"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/platform/usage"
)

func TestWorkflowAgentStepFailsOnExhaustion(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "aura.db")
	sqlitetest.Seed(t, dsn, Schemas()...)
	db, err := sqlite.Open(t.Context(), dsn, sqlite.Options{})
	if err != nil {
		t.Fatalf("open db: %v", err)
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
		t.Fatalf("put price: %v", err)
	}
	ledger, err := usage.NewTurnLedger(cfg.Usage, cfg.Access, db, prices, nil)
	if err != nil {
		t.Fatalf("NewTurnLedger: %v", err)
	}
	broker := toolbrokertest.NewBroker(t, &toolbroker.Options{HostCapabilities: capabilitytest.Host(t)})
	registerStubBuiltin(t, broker, "read_file", false, func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
		return toolbroker.ToolResult{}, errors.New("file not readable")
	})
	builtin, err := runtimeadk.NewBrokerExecutor(broker, capabilitytest.Host(t))
	if err != nil {
		t.Fatalf("NewBrokerExecutor: %v", err)
	}

	modelLog := &agentModelLog{}
	readCall := &genai.FunctionCall{Name: "read_file", Args: map[string]any{"path": "a.txt"}}
	script := &agentScriptModel{
		arrive: modelLog.arrive,
		steps: []agentScriptStep{
			{call: readCall},
			{call: readCall},
			{call: readCall},
			{call: readCall},
			{text: "Sorry, I could not read the file."},
		},
	}
	stepExecutor, err := newAgentStepExecutor(&agentStepExecutorInput{
		models: auramodel.Set{"primary": script}, modelName: "primary", sessions: turnstore.NewSessionService(db),
		builtin: builtin, ledger: ledger, registry: registry,
		routeModel: func(string) (string, error) { return "primary", nil },
		toolBudget: toolBudgetFromRuntime(cfg.Runtime),
		logger:     slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("newAgentStepExecutor: %v", err)
	}

	wfStore, err := workflow.NewStore(db, turnstore.NewTurnStore(db))
	if err != nil {
		t.Fatalf("workflow.NewStore: %v", err)
	}
	validation := &workflow.ValidationDeps{
		Agents:     registry,
		KnownTools: []string{"read_file"},
		StepAllowed: func(string, principal.Class, string) (bool, bool) {
			return true, true
		},
	}
	agentRunner, err := workflow.NewAgentTurnRunner(&workflow.AgentTurnRunnerDeps{
		Turns:      stepExecutor,
		Events:     turnstore.NewEventStore(db),
		Sessions:   turnstore.NewSessionService(db),
		Tools:      broker,
		Runs:       wfStore,
		Validation: validation,
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("NewAgentTurnRunner: %v", err)
	}

	fake := durabletest.NewFake()
	interp, err := workflow.NewInterpreter(wfStore, fake, &workflow.Options{
		MaxConcurrentSteps: 1,
		AgentSteps:         agentRunner,
		Driver:             workflow.DriverSerial,
		Logger:             slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("NewInterpreter: %v", err)
	}

	personaID := "researcher"
	spec := &workflow.Spec{
		ID:      "budget-wf",
		Goal:    "Read a file",
		Version: 1,
		Source:  workflow.SourceDefined,
		Steps: []workflow.StepSpec{{
			ID:      "gather",
			Timeout: time.Minute,
			Executor: workflow.ExecutorSpec{
				Kind:    workflow.KindAgent,
				AgentID: &personaID,
				Prompt:  "Read a.txt",
				Tools:   []string{"read_file"},
			},
		}},
	}
	if err := interp.Install(t.Context(), []*workflow.Spec{spec}, validation); err != nil {
		t.Fatalf("Install: %v", err)
	}
	origin := &workflow.RunOrigin{PrincipalID: principal.OwnerID, PrincipalClass: string(principal.ClassOwner), AgentID: "main"}
	summary, err := interp.Start(t.Context(), spec.ID, &workflow.RunInput{Objective: "test"}, origin)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, err := wfStore.Run(t.Context(), summary.ID)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if got.Status == workflow.RunFailed || got.Status == workflow.RunSucceeded {
			break
		}
		select {
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		case <-time.After(10 * time.Millisecond):
		}
	}

	run, err := wfStore.Run(t.Context(), summary.ID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.Status != workflow.RunFailed {
		t.Fatalf("run status = %s, want %s", run.Status, workflow.RunFailed)
	}
	steps, err := wfStore.Steps(t.Context(), summary.ID)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(steps))
	}
	if steps[0].Status != workflow.StepFailed || steps[0].ErrorCode != string(workflow.ErrorCodeToolBudgetExhausted) {
		t.Fatalf("step = %+v, want failed with %s", steps[0], workflow.ErrorCodeToolBudgetExhausted)
	}
	if len(steps[0].Output) != 0 {
		t.Fatalf("step output = %s, want empty (apology must not be stored as success)", steps[0].Output)
	}
	if got := len(modelLog.seen()); got != 4 {
		t.Fatalf("model calls = %d, want 4 (step must end before a 5th apology call)", got)
	}
}

func TestAllWiringSitesPassToolBudget(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "aura.db")
	sqlitetest.Seed(t, dsn, Schemas()...)
	db, err := sqlite.Open(t.Context(), dsn, sqlite.Options{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := appconfig.Default()
	cfg.Runtime.TurnTimeout = configkit.Duration(2 * time.Minute)
	cfg.Runtime.AnswerReserve = configkit.Duration(12 * time.Second)
	cfg.Runtime.MaxConsecutiveToolErrors = 2
	cfg.Runtime.MaxToolErrorsPerTurn = 5
	cfg.Models.Definitions["primary"] = auramodel.Definition{Model: "primary-model"}
	want := runtimeadk.ToolBudget{
		AnswerReserve:            12 * time.Second,
		MaxConsecutiveToolErrors: 2,
		MaxToolErrorsPerTurn:     5,
	}

	registry, err := auraagent.Build(nil, definitionNames(), appconfig.BuiltinToolDefinitions(), []string{"primary"}, "en")
	if err != nil {
		t.Fatalf("agent registry: %v", err)
	}
	prices := usage.NewPriceRegistry()
	if err := prices.Put(&usage.Price{
		ModelDefinitionID: "primary", Currency: cfg.Usage.Currency, MicrosPerInputToken: 1, MicrosPerOutputToken: 1,
		MaxReservationRate: 100, EffectiveFrom: time.Unix(0, 0).UTC(),
	}); err != nil {
		t.Fatalf("put price: %v", err)
	}
	ledger, err := usage.NewTurnLedger(cfg.Usage, cfg.Access, db, prices, nil)
	if err != nil {
		t.Fatalf("NewTurnLedger: %v", err)
	}
	broker := toolbrokertest.NewBroker(t, &toolbroker.Options{HostCapabilities: capabilitytest.Host(t)})
	registerStubBuiltin(t, broker, "read_file", false, func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
		return toolbroker.ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
	})
	builtin, err := runtimeadk.NewBrokerExecutor(broker, capabilitytest.Host(t))
	if err != nil {
		t.Fatalf("NewBrokerExecutor: %v", err)
	}
	evalModel := &personaEvalModel{}
	models := auramodel.Set{"primary-model": evalModel}
	modelReg := &auramodel.Registry{}

	b := &build{
		opts:       &Options{Surface: SurfaceChat},
		cfg:        &cfg,
		db:         db,
		models:     modelReg,
		sessions:   turnstore.NewSessionService(db),
		events:     turnstore.NewEventStore(db),
		turns:      turnstore.NewTurnStore(db),
		builtin:    builtin,
		ledger:     ledger,
		registry:   registry,
		routeModel: func(string) (string, error) { return "primary-model", nil },
		liveEvents: turn.NewEventBus(nil),
		logger:     slog.New(slog.DiscardHandler),
	}

	t.Run("server turn runtime", func(t *testing.T) {
		if err := b.buildTurnRuntime(); err != nil {
			t.Fatalf("buildTurnRuntime: %v", err)
		}
		if got := b.executor.ToolBudget(); got != want {
			t.Fatalf("server executor ToolBudget = %+v, want %+v", got, want)
		}
	})

	t.Run("workflow agent step executor", func(t *testing.T) {
		exec, err := b.newAgentStepExecutor()
		if err != nil {
			t.Fatalf("newAgentStepExecutor: %v", err)
		}
		if got := exec.ToolBudget(); got != want {
			t.Fatalf("workflow step executor ToolBudget = %+v, want %+v", got, want)
		}
	})

	t.Run("persona eval behavior executor", func(t *testing.T) {
		routed, err := routedBehaviorModels(&cfg, models)
		if err != nil {
			t.Fatalf("routedBehaviorModels: %v", err)
		}
		def, err := registry.Resolve(nil, nil)
		if err != nil {
			t.Fatalf("resolve default persona: %v", err)
		}
		exec, err := routed.newExecutor(turnstore.NewSessionService(db), &def)
		if err != nil {
			t.Fatalf("behaviorModels.newExecutor: %v", err)
		}
		if got := exec.ToolBudget(); got != want {
			t.Fatalf("persona eval behavior executor ToolBudget = %+v, want %+v", got, want)
		}
	})

	t.Run("persona eval capture executor", func(t *testing.T) {
		def, err := registry.Resolve(nil, nil)
		if err != nil {
			t.Fatalf("resolve default persona: %v", err)
		}
		exec, err := newPersonaCapturer(&cfg).newExecutor(turnstore.NewSessionService(db), evalModel, &def)
		if err != nil {
			t.Fatalf("personaCapturer.newExecutor: %v", err)
		}
		if got := exec.ToolBudget(); got != want {
			t.Fatalf("persona eval capture executor ToolBudget = %+v, want %+v", got, want)
		}
	})
}
