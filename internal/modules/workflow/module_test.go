package workflow

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/durable/durabletest"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

type moduleBroker struct{ hostTools }

func (moduleBroker) StandingApprovalAllowed(context.Context, *toolbroker.ToolRequest) (bool, error) {
	return false, nil
}

func (moduleBroker) ExactCallText(name string, _ json.RawMessage) (string, error) { return name, nil }

func (moduleBroker) GrantUntil(context.Context, *toolbroker.ToolRequest, time.Time) (approval.ApprovalGrant, error) {
	return approval.ApprovalGrant{}, nil
}

func (moduleBroker) Execute(context.Context, *toolbroker.ToolRequest) (toolbroker.ToolResult, error) {
	return toolbroker.ToolResult{}, nil
}

type moduleHarness struct {
	agentHarness
	mod *Module
}

func newModuleHarness(t *testing.T, withBroker bool) *moduleHarness {
	t.Helper()
	db := newTestDB(t)
	fake := durabletest.NewFake()
	deps := testValidationDeps()
	moduleDeps := &ModuleDeps{
		DB: db, Sessions: turnstore.NewTurnStore(db), RunSessions: turnstore.NewSessionService(db),
		Durable: fake, ValidationDeps: deps,
	}
	if withBroker {
		registry, err := buildTestRegistry()
		if err != nil {
			t.Fatal(err)
		}
		moduleDeps.Broker, moduleDeps.Registry = moduleBroker{hostTools{"read_file": true}}, registry
	}
	mod, err := New(t.Context(), moduleDeps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := &moduleHarness{mod: mod}
	h.db, h.disk, h.fake, h.deps, h.origin = db, mod.Store(), fake, deps, ownerOrigin()
	h.turns = &scriptedTurns{replies: finalAnswer("module answer")}
	h.interp = mod.Interpreter()
	return h
}

func TestAgentStepFailsWhenNoAgentTurnsAreBound(t *testing.T) {
	h := newModuleHarness(t, true)
	runID, terminal := h.drive(t, agentSpec("unbound", agentStep("s", agentExec("go", ""))))
	if terminal != RunFailed {
		t.Fatalf("run = %s, want %s", terminal, RunFailed)
	}
	if step := h.step(t, runID, "s"); step.ErrorCode != string(ErrorCodeExecutorInvalid) {
		t.Fatalf("step error = %q, want %s", step.ErrorCode, ErrorCodeExecutorInvalid)
	}
	if requests, models, toolRuns := h.turns.snapshot(); len(requests) != 0 || models != 0 || toolRuns != 0 {
		t.Fatalf("an unbound module made %d requests, %d model calls and %d tool runs", len(requests), models, toolRuns)
	}
}

func TestBoundModuleRunsAnAgentStep(t *testing.T) {
	h := newModuleHarness(t, true)
	if err := h.mod.BindAgentTurns(h.turns, turnstore.NewEventStore(h.db)); err != nil {
		t.Fatalf("BindAgentTurns: %v", err)
	}
	runID, terminal := h.drive(t, agentSpec("bound", agentStep("s", agentExec("go", ""))))
	if terminal != RunSucceeded {
		t.Fatalf("run = %s, want %s", terminal, RunSucceeded)
	}
	var output agentOutput
	if err := json.Unmarshal(h.step(t, runID, "s").Output, &output); err != nil || output.Text != "module answer" {
		t.Fatalf("step output = %+v, %v, want the model's final text", output, err)
	}
}

func TestBindAgentTurnsFailsClosed(t *testing.T) {
	cases := []struct {
		name       string
		withBroker bool
		turns      func(h *moduleHarness) StepTurns
		events     func(h *moduleHarness) StepEvents
		bindFirst  bool
	}{
		{name: "no turns", withBroker: true, turns: func(*moduleHarness) StepTurns { return nil }, events: eventsOf},
		{name: "no events", withBroker: true, turns: turnsOf, events: func(*moduleHarness) StepEvents { return nil }},
		{name: "a module built without a broker", turns: turnsOf, events: eventsOf},
		{name: "a second binding", withBroker: true, turns: turnsOf, events: eventsOf, bindFirst: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newModuleHarness(t, tc.withBroker)
			if tc.bindFirst {
				if err := h.mod.BindAgentTurns(turnsOf(h), eventsOf(h)); err != nil {
					t.Fatalf("first BindAgentTurns: %v", err)
				}
			}
			if err := h.mod.BindAgentTurns(tc.turns(h), tc.events(h)); !hasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("BindAgentTurns = %v, want %s", err, ErrorCodeInvalidArgument)
			}
		})
	}
}

func turnsOf(h *moduleHarness) StepTurns { return h.turns }

func eventsOf(h *moduleHarness) StepEvents { return turnstore.NewEventStore(h.db) }
