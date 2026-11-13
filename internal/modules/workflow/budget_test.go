package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/durable"
)

func threeToolSteps(id string, source Source) *Spec {
	read := "read_file"
	spec := &Spec{ID: id, Goal: "Read three times", Version: 1, Source: source}
	for index, stepID := range []string{"one", "two", "three"} {
		step := StepSpec{ID: stepID, Executor: ExecutorSpec{Kind: KindTool, ToolID: &read, ToolArgs: json.RawMessage(`{"path":"a"}`)}, Timeout: 5 * time.Second}
		if index > 0 {
			step.DependsOn = []string{spec.Steps[index-1].ID}
		}
		spec.Steps = append(spec.Steps, step)
	}
	return spec
}

func budgetInterpreter(t *testing.T, budget Budget, options *Options) (*Interpreter, *Store, *durable.Fake) {
	t.Helper()
	disk := newTestStore(t)
	fake := durable.NewFake()
	options.GeneratedBudget = budget
	options.MaxConcurrentSteps = 2
	options.Driver = DriverConcurrent
	return NewInterpreter(disk, fake, options), disk, fake
}

func stepByID(t *testing.T, disk *Store, runID, stepID string) *StepRun {
	t.Helper()
	steps, err := disk.Steps(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.StepID == stepID {
			return step
		}
	}
	t.Fatalf("step %s not found", stepID)
	return nil
}

func TestGeneratedRunsStopAtMaxStepsAndDefinedRunsDoNot(t *testing.T) {
	tools := &fakeToolRunner{}
	interpreter, disk, _ := budgetInterpreter(t, Budget{MaxSteps: 2, MaxTokens: 1000}, &Options{Tools: tools})
	installAll(t, interpreter, threeToolSteps("gen_prop_a", SourceGenerated), threeToolSteps("defined", SourceDefined))
	generated, err := interpreter.Start(context.Background(), "gen_prop_a", nil, ownerOrigin())
	if err != nil {
		t.Fatal(err)
	}
	waitForRunStatus(t, disk, generated.ID, RunFailed, 5*time.Second)
	if step := stepByID(t, disk, generated.ID, "three"); step.Status != StepFailed || step.ErrorCode != string(ErrorCodeBudgetExhausted) {
		t.Fatalf("third step = %+v", step)
	}
	if calls := len(tools.called); calls != 2 {
		t.Fatalf("tool calls = %d, want 2 within the budget", calls)
	}
	defined, err := interpreter.Start(context.Background(), "defined", nil, ownerOrigin())
	if err != nil {
		t.Fatal(err)
	}
	waitForRunStatus(t, disk, defined.ID, RunSucceeded, 5*time.Second)
}

type flakyTools struct {
	mu    sync.Mutex
	calls int
}

func (f *flakyTools) Invoke(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return nil, errors.New("still failing")
}

func TestRetriesAndChildStepsCountTowardTheParentBudget(t *testing.T) {
	retrying := &Spec{ID: "gen_prop_retry", Goal: "Retry", Version: 1, Source: SourceGenerated, Steps: []StepSpec{
		{ID: "flaky", Executor: ExecutorSpec{Kind: KindTool, ToolID: ptr("read_file"), ToolArgs: json.RawMessage(`{"path":"a"}`)}, Timeout: 5 * time.Second, Retry: RetryPolicy{Attempts: 4}},
	}}
	tools := &flakyTools{}
	interpreter, disk, _ := budgetInterpreter(t, Budget{MaxSteps: 3}, &Options{Tools: tools})
	installAll(t, interpreter, retrying)
	run, err := interpreter.Start(context.Background(), retrying.ID, nil, ownerOrigin())
	if err != nil {
		t.Fatal(err)
	}
	waitForRunStatus(t, disk, run.ID, RunFailed, 5*time.Second)
	if tools.calls != 3 || stepByID(t, disk, run.ID, "flaky").ErrorCode != string(ErrorCodeBudgetExhausted) {
		t.Fatalf("calls = %d, step = %+v", tools.calls, stepByID(t, disk, run.ID, "flaky"))
	}

	counting := &fakeToolRunner{}
	parentInterpreter, parentDisk, _ := budgetInterpreter(t, Budget{MaxSteps: 3}, &Options{Tools: counting})
	parent := composingSpec("gen_prop_parent", "defined")
	parent.Source = SourceGenerated
	installAll(t, parentInterpreter, threeToolSteps("defined", SourceDefined), parent)
	parentRun, err := parentInterpreter.Start(context.Background(), parent.ID, nil, ownerOrigin())
	if err != nil {
		t.Fatal(err)
	}
	waitForRunStatus(t, parentDisk, parentRun.ID, RunFailed, 5*time.Second)
	if calls := len(counting.called); calls != 2 {
		t.Fatalf("child tool calls = %d, want 2: the composing step uses one of the three", calls)
	}
}

type meteredAgents struct{ tokens int64 }

func (meteredAgents) Run(context.Context, *auraagent.Definition, *ExecutionContext) (json.RawMessage, error) {
	return nil, errors.New("the unmetered path must not be used")
}

func (m meteredAgents) RunMetered(context.Context, *auraagent.Definition, *ExecutionContext) (json.RawMessage, int64, error) {
	return json.RawMessage(`{"ok":true}`), m.tokens, nil
}

func agentSteps(id string) *Spec {
	spec := &Spec{ID: id, Goal: "Think twice", Version: 1, Source: SourceGenerated}
	for index, stepID := range []string{"first", "second", "third"} {
		step := StepSpec{ID: stepID, Executor: ExecutorSpec{Kind: KindAgent, AgentID: ptr("engineer")}, Timeout: 5 * time.Second}
		if index > 0 {
			step.DependsOn = []string{spec.Steps[index-1].ID}
		}
		spec.Steps = append(spec.Steps, step)
	}
	return spec
}

func TestMeteredAgentTokensStopTheRunInBothDrivers(t *testing.T) {
	registry, err := buildTestRegistry()
	if err != nil {
		t.Fatal(err)
	}
	interpreter, disk, fake := budgetInterpreter(t, Budget{MaxSteps: 10, MaxTokens: 250}, &Options{Agents: meteredAgents{tokens: 150}, AgentResolver: registry})
	installAll(t, interpreter, agentSteps("gen_prop_tokens"), agentSteps("gen_prop_serial"))
	run, err := interpreter.Start(context.Background(), "gen_prop_tokens", nil, ownerOrigin())
	if err != nil {
		t.Fatal(err)
	}
	waitForRunStatus(t, disk, run.ID, RunFailed, 5*time.Second)
	if step := stepByID(t, disk, run.ID, "second"); step.ErrorCode != string(ErrorCodeBudgetExhausted) {
		t.Fatalf("second step = %+v", step)
	}
	if third := stepByID(t, disk, run.ID, "third"); third.Status == StepSucceeded {
		t.Fatal("a step ran after the token budget was spent")
	}
	serialRun, err := disk.newRun(context.Background(), specOf(t, interpreter, "gen_prop_serial"), &RunInput{}, ownerOrigin())
	if err != nil {
		t.Fatal(err)
	}
	inv, _ := holdInvocation(t, fake, "holder-tokens")
	terminal, err := interpreter.DriveSerial(context.Background(), inv, serialRun.ID)
	if terminal != RunFailed && err == nil {
		t.Fatalf("serial token budget = %s, %v", terminal, err)
	}
	if step := stepByID(t, disk, serialRun.ID, "second"); step.ErrorCode != string(ErrorCodeBudgetExhausted) {
		t.Fatalf("serial second step = %+v", step)
	}
}

func TestProposalLongerThanMaxStepsIsRejected(t *testing.T) {
	interpreter, _, _ := budgetInterpreter(t, Budget{MaxSteps: 2}, &Options{Tools: &fakeToolRunner{}})
	installAll(t, interpreter, toolSpec("leaf"))
	if _, err := interpreter.CompileProposal(NormalizeProposal(threeToolSteps("x", SourceGenerated), "prop_long"), []string{"read_file"}); !hasCode(err, ErrorCodeBudgetExhausted) {
		t.Fatalf("CompileProposal of a 3-step spec under max_steps 2 = %v", err)
	}
}
