//go:build durable

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

const (
	effectTool         = "stub-effect"
	afterEffectTool    = "stub-after"
	effectStepID       = "effect"
	effectReturnedGate = "effect-returned"
)

var effectReturnedWorkflow = &workflowShape{
	spec:      effectSpec,
	known:     []string{effectTool, afterEffectTool},
	effectful: []string{effectTool},
	stubs:     []workflowStub{{tool: stubTool{name: effectTool}, effectful: true}, {tool: stubTool{name: afterEffectTool}}},
	wire:      gateAfterTheEffect,
}

var effectInterruptedWorkflow = &workflowShape{
	spec:      effectSpec,
	known:     []string{effectTool, afterEffectTool},
	effectful: []string{effectTool},
	stubs:     []workflowStub{{tool: stubTool{name: effectTool, gatedInAdapter: true}, effectful: true}, {tool: stubTool{name: afterEffectTool}}},
}

func effectSpec() *workflow.Spec {
	effectID, afterID := effectTool, afterEffectTool
	return &workflow.Spec{
		ID: "effect-once", Goal: "Perform an effect once across a killed process", Version: 1, Source: workflow.SourceDefined,
		Steps: []workflow.StepSpec{
			{ID: "gate", Executor: workflow.ExecutorSpec{Kind: workflow.KindApproval}, Timeout: 10 * time.Minute},
			{
				ID: effectStepID, DependsOn: []string{"gate"},
				Executor: workflow.ExecutorSpec{Kind: workflow.KindTool, ToolID: &effectID, ToolArgs: json.RawMessage(`{"note":"once"}`)},
				Timeout:  2 * time.Minute,
				Retry:    workflow.RetryPolicy{Attempts: 1, Backoff: 10 * time.Millisecond},
			},
			{
				ID: "after", DependsOn: []string{effectStepID},
				Executor: workflow.ExecutorSpec{Kind: workflow.KindTool, ToolID: &afterID, ToolArgs: json.RawMessage(`{"note":"after"}`)},
				Timeout:  30 * time.Second,
			},
		},
	}
}

type gatedToolRunner struct {
	inner  workflow.ToolRunner
	ledger *turnLedger
	gate   string
}

func (r *gatedToolRunner) Invoke(ctx context.Context, toolID string, args json.RawMessage) (json.RawMessage, error) {
	output, err := r.inner.Invoke(ctx, toolID, args)
	if toolID != effectTool {
		return output, err
	}
	if gateErr := r.ledger.gate(ctx, r.gate); gateErr != nil {
		return nil, gateErr
	}
	return output, err
}

func gateAfterTheEffect(_ *testing.T, life *runLife, runner *workflowToolRunner) workflow.ToolRunner {
	return &gatedToolRunner{inner: runner, ledger: life.ledger, gate: effectReturnedGate}
}

type effectKill struct {
	scenario string
	gate     string
	intent   effect.State
	executed int
	replay   func(t *testing.T, suite *runSuite)
}

func TestDurableWorkflowDoesNotRepeatAnEffectThatSucceededBeforeAKill(t *testing.T) {
	runEffectKill(t, effectKill{
		scenario: "effect-returned", gate: effectReturnedGate,
		intent: effect.StateSucceeded, executed: 1, replay: requireReturnedEffectOutcome,
	})
}

func TestDurableWorkflowDoesNotRepeatAnEffectInterruptedByAKill(t *testing.T) {
	runEffectKill(t, effectKill{
		scenario: "effect-interrupted", gate: effectTool,
		intent: effect.StateStarted, executed: 0, replay: requireInterruptedEffectOutcome,
	})
}

func runEffectKill(t *testing.T, kill effectKill) {
	t.Helper()
	suite := startRunSuite(t, runStart{scenario: kill.scenario})
	suite.child.waitKillPoint(t, "the first life is inside the effectful step", suite.ledger.exists(t, kill.gate+".entered"))
	suite.requireKilledInsideTheEffect(t, kill)

	suite.child.kill(t)
	suite.release(t, kill.gate)
	suite.openSecondLife(t)

	suite.requireRunSucceeded(t)
	suite.requireWorkflowHandler(t)
	suite.ledger.requireCount(t, executions(effectTool), kill.executed)
	suite.requireOneEffectIntent(t, kill.intent)
	kill.replay(t, suite)
}

func (s *runSuite) requireKilledInsideTheEffect(t *testing.T, kill effectKill) {
	t.Helper()
	if n := len(s.ledger.records(t, executions(effectTool))); n != kill.executed {
		t.Fatalf("%s executions at the kill = %d, want %d", effectTool, n, kill.executed)
	}
	s.requireOneEffectIntent(t, kill.intent)
	action := s.effectAction(0)
	if n := countRows(t, s.db, `SELECT COUNT(*) FROM durable_action WHERE run_key = ? AND action_key = ?`, s.plan.Key, action); n != 0 {
		t.Fatalf("journal actions %q at the kill = %d, want 0", action, n)
	}
}

func (s *runSuite) effectAction(attempt int) string {
	return "run/" + s.plan.Run + "/step/" + effectStepID + "/attempt/" + strconv.Itoa(attempt)
}

func (s *runSuite) requireOneEffectIntent(t *testing.T, state effect.State) {
	t.Helper()
	rows, err := s.db.QueryContext(t.Context(), `SELECT state, idempotency_key FROM effect_intent WHERE operation = ?`, effectTool)
	if err != nil {
		t.Fatalf("read the effect intents of %s: %v", effectTool, err)
	}
	defer func() { _ = rows.Close() }()
	type intent struct{ state, key string }
	var intents []intent
	for rows.Next() {
		var found intent
		if err := rows.Scan(&found.state, &found.key); err != nil {
			t.Fatalf("scan an effect intent: %v", err)
		}
		intents = append(intents, found)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the effect intents of %s: %v", effectTool, err)
	}
	want := intent{state: string(state), key: "wf/" + s.plan.Run + "/" + effectStepID}
	if len(intents) != 1 || intents[0] != want {
		t.Fatalf("effect intents of %s = %+v, want exactly %+v", effectTool, intents, want)
	}
}

type journaledAction struct {
	Status    string `json:"status"`
	ErrorCode string `json:"error_code"`
	Detail    string `json:"detail"`
}

func (s *runSuite) journaledAttempt(t *testing.T, attempt int) (journaledAction, bool) {
	t.Helper()
	var raw []byte
	err := s.db.QueryRowContext(t.Context(), `SELECT result FROM durable_action WHERE run_key = ? AND action_key = ?`, s.plan.Key, s.effectAction(attempt)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return journaledAction{}, false
	}
	if err != nil {
		t.Fatalf("read the journal of attempt %d: %v", attempt, err)
	}
	var record journaledAction
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("decode the journal of attempt %d: %v", attempt, err)
	}
	return record, true
}

func requireReturnedEffectOutcome(t *testing.T, suite *runSuite) {
	t.Helper()
	disk := workflow.NewStore(suite.db)
	if status := workflowStatus(t, disk, suite.plan.Run); status != workflow.RunSucceeded {
		t.Errorf("workflow run status = %q, want %q", status, workflow.RunSucceeded)
	}
	requireStepsSucceeded(t, disk, suite.plan.Run)
	steps, err := disk.Steps(t.Context(), suite.plan.Run)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	effectSteps := 0
	for _, step := range steps {
		if step.StepID != effectStepID {
			continue
		}
		effectSteps++
		if string(step.Output) != `{"ok":true}` {
			t.Errorf("step %s output = %s, want %s", step.StepID, step.Output, `{"ok":true}`)
		}
	}
	if effectSteps != 1 {
		t.Errorf("step runs of %s = %d, want 1", effectStepID, effectSteps)
	}
	var raw []byte
	if err := suite.db.QueryRowContext(t.Context(), `SELECT result FROM durable_action WHERE run_key = ? AND action_key = ?`, suite.plan.Key, suite.effectAction(0)).Scan(&raw); err != nil {
		t.Fatalf("read the journal of attempt 0: %v", err)
	}
	var record struct {
		Status  string          `json:"status"`
		Attempt int             `json:"attempt"`
		Output  json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("decode the journal of attempt 0: %v", err)
	}
	if record.Status != workflow.StepSucceeded || record.Attempt != 1 || string(record.Output) != `{"ok":true}` {
		t.Errorf("journal of attempt 0 = %s %d %s, want %s 1 %s", record.Status, record.Attempt, record.Output, workflow.StepSucceeded, `{"ok":true}`)
	}
	if _, found := suite.journaledAttempt(t, 1); found {
		t.Error("the journal holds an attempt 1, want none")
	}
	suite.ledger.requireCount(t, executions(afterEffectTool), 1)
}

func requireInterruptedEffectOutcome(t *testing.T, suite *runSuite) {
	t.Helper()
	suite.requireEffectStepRefused(t, "effectful tool settled as started")
}

func (s *runSuite) requireEffectStepRefused(t *testing.T, phrase string) {
	t.Helper()
	disk := workflow.NewStore(s.db)
	if status := workflowStatus(t, disk, s.plan.Run); status != workflow.RunFailed {
		t.Errorf("workflow run status = %q, want %q", status, workflow.RunFailed)
	}
	steps, err := disk.Steps(t.Context(), s.plan.Run)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	effectSteps := 0
	for _, step := range steps {
		if step.StepID != effectStepID {
			continue
		}
		effectSteps++
		if step.Status != workflow.StepFailed || step.ErrorCode != string(workflow.ErrorCodeStepFailed) {
			t.Errorf("step %s = %s with %q, want %s with %q", step.StepID, step.Status, step.ErrorCode, workflow.StepFailed, workflow.ErrorCodeStepFailed)
		}
	}
	if effectSteps != 1 {
		t.Errorf("step runs of %s = %d, want 1", effectStepID, effectSteps)
	}
	class := string(toolbroker.ResultExecutionFailed)
	want := journaledAction{Status: workflow.StepFailed, ErrorCode: string(workflow.ErrorCodeStepFailed), Detail: class + ": " + class + ": " + phrase}
	for attempt := range 2 {
		record, found := s.journaledAttempt(t, attempt)
		if !found || record != want {
			t.Errorf("journal of attempt %d = %+v (found %v), want %+v", attempt, record, found, want)
		}
	}
	if _, found := s.journaledAttempt(t, 2); found {
		t.Error("the journal holds an attempt 2, want none")
	}
	s.ledger.requireCount(t, executions(afterEffectTool), 0)
}
