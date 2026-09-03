package workflow

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/durable"
	"github.com/anggasct/aura/internal/store"
)

func buildTestRegistry() (*auraagent.Registry, error) {
	return auraagent.Build(nil, []string{"read_file", "write_file", "exec", "list_dir", "web_fetch", "web_search"}, []string{"primary"})
}

type fakeAgentRunner struct {
	mu       sync.Mutex
	calls    int
	output   json.RawMessage
	inFlight chan struct{}
}

func (f *fakeAgentRunner) Run(ctx context.Context, definition *auraagent.Definition, input *ExecutionContext) (json.RawMessage, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.inFlight != nil {
		<-f.inFlight
	}
	if f.output != nil {
		return f.output, nil
	}
	return json.RawMessage(`{"status":"done"}`), nil
}

type fakeToolRunner struct {
	mu     sync.Mutex
	called []string
	output func(toolID string) json.RawMessage
}

func (f *fakeToolRunner) Invoke(ctx context.Context, toolID string, args json.RawMessage) (json.RawMessage, error) {
	f.mu.Lock()
	f.called = append(f.called, toolID)
	f.mu.Unlock()
	if f.output != nil {
		return f.output(toolID), nil
	}
	return json.RawMessage(`{"ok":true}`), nil
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	db, err := store.OpenDB(ctx, filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewStore(db)
}

func TestInterpreterRunsMixedWorkflowEndToEnd(t *testing.T) {
	ctx := context.Background()
	registry, err := buildTestRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	fake := durable.NewFake().WithLogger(func(format string, args ...any) {
		t.Logf(format, args...)
	})
	disk := newTestStore(t)
	agents := &fakeAgentRunner{output: json.RawMessage(`{"decision":"approve"}`)}
	tools := &fakeToolRunner{output: func(toolID string) json.RawMessage {
		return json.RawMessage(`{"pr":123}`)
	}}
	interpreter := NewInterpreter(disk, fake, &Options{
		MaxConcurrentSteps: 2,
		AgentResolver:      registry,
		Agents:             agents,
		Tools:              tools,
	})
	tool := "read_file"
	spec := &Spec{
		ID: "mixed", Goal: "Mixed E2E", Version: 1, Source: SourceDefined,
		Steps: []StepSpec{
			{ID: "implement", Executor: ExecutorSpec{Kind: KindAgent, AgentID: ptr("engineer")}, Timeout: 5 * time.Second},
			{ID: "verify", DependsOn: []string{"implement"}, Executor: ExecutorSpec{Kind: KindAgent, RequiredCapabilities: []string{auraagent.CapabilityRepositoryRead}}, Timeout: 5 * time.Second},
			{ID: "record", DependsOn: []string{"verify"}, Executor: ExecutorSpec{Kind: KindTool, ToolID: &tool}, Timeout: 5 * time.Second},
			{ID: "approve", DependsOn: []string{"record"}, Executor: ExecutorSpec{Kind: KindApproval}, Timeout: 5 * time.Second},
			{
				ID: "close", DependsOn: []string{"approve"},
				Condition: strPtr(`steps.approve.output.decision == "approve" && steps.record.output.pr == 123`),
				Executor:  ExecutorSpec{Kind: KindApproval}, Timeout: 5 * time.Second,
			},
		},
	}
	if err := interpreter.Load(ctx, spec, testValidationDeps()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	summary, err := interpreter.Start(ctx, "mixed", &RunInput{Objective: "ship it"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The approval step suspends; resolve it through the bound signal.
	signalAndSettle := func(name string, payload string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if err := fake.Signal(ctx, durable.RunRef{Key: summary.DurableKey}, name, []byte(payload)); err == nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("signal %s never reached a receptive run", name)
	}
	waitForRunStatus(t, disk, summary.ID, RunSuspended, 2*time.Second)
	signalAndSettle("approval.approve", `{"decision":"approve"}`)
	waitForRunStatus(t, disk, summary.ID, RunSuspended, 2*time.Second)
	signalAndSettle("approval.close", `{"decision":"approve"}`)
	waitForRunStatus(t, disk, summary.ID, RunSucceeded, 2*time.Second)

	steps, err := disk.Steps(ctx, summary.ID)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	statuses := map[string]string{}
	for _, step := range steps {
		statuses[step.StepID] = step.Status
	}
	for stepID, want := range map[string]string{
		"implement": StepSucceeded, "verify": StepSucceeded, "record": StepSucceeded,
		"approve": StepSucceeded, "close": StepSucceeded,
	} {
		if statuses[stepID] != want {
			t.Errorf("step %s = %s, want %s", stepID, statuses[stepID], want)
		}
	}
	if agents.callCount() == 0 {
		t.Error("agent runner never ran")
	}
	if len(tools.snapshotCalled()) == 0 {
		t.Error("tool runner never ran")
	}
}

func TestInterpreterSkipsDownstreamOnFalseCondition(t *testing.T) {
	ctx := context.Background()
	registry, err := buildTestRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	fake := durable.NewFake()
	disk := newTestStore(t)
	agents := &fakeAgentRunner{output: json.RawMessage(`{"decision":"reject"}`)}
	interpreter := NewInterpreter(disk, fake, &Options{
		MaxConcurrentSteps: 2,
		AgentResolver:      registry,
		Agents:             agents,
	})
	spec := &Spec{
		ID: "skipflow", Goal: "Skip", Version: 1, Source: SourceDefined,
		Steps: []StepSpec{
			{ID: "decide", Executor: ExecutorSpec{Kind: KindAgent, AgentID: ptr("main")}, Timeout: 5 * time.Second},
			{
				ID: "merge", DependsOn: []string{"decide"},
				Condition: strPtr(`steps.decide.output.decision == "approve"`),
				Executor:  ExecutorSpec{Kind: KindApproval}, Timeout: 5 * time.Second,
			},
			{ID: "notify", DependsOn: []string{"merge"}, Executor: ExecutorSpec{Kind: KindApproval}, Timeout: 5 * time.Second},
		},
	}
	if err := interpreter.Load(ctx, spec, ValidationDeps{Agents: registry}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	summary, err := interpreter.Start(ctx, "skipflow", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForRunStatus(t, disk, summary.ID, RunSucceeded, 2*time.Second)
	steps, err := disk.Steps(ctx, summary.ID)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	for _, step := range steps {
		want := StepSucceeded
		if step.StepID != "decide" {
			want = StepSkipped
		}
		if step.Status != want {
			t.Errorf("step %s = %s, want %s", step.StepID, step.Status, want)
		}
	}
}

func TestInterpreterStepTimeoutRetriesThenFailsRun(t *testing.T) {
	ctx := context.Background()
	registry, err := buildTestRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	clock := durable.NewManualClock(time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC))
	fake := durable.NewFake().WithClock(clock)
	disk := newTestStore(t)
	block := make(chan struct{})
	agents := &fakeAgentRunner{inFlight: block}
	interpreter := NewInterpreter(disk, fake, &Options{
		MaxConcurrentSteps: 1,
		AgentResolver:      registry,
		Agents:             agents,
	})
	spec := &Spec{
		ID: "slowflow", Goal: "Slow", Version: 1, Source: SourceDefined,
		Steps: []StepSpec{
			{ID: "work", Executor: ExecutorSpec{Kind: KindAgent, AgentID: ptr("main")}, Timeout: time.Minute, Retry: RetryPolicy{Attempts: 2, Backoff: time.Minute}},
		},
	}
	if err := interpreter.Load(ctx, spec, ValidationDeps{Agents: registry}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	summary, err := interpreter.Start(ctx, "slowflow", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Each attempt burns the step timeout on the runtime clock; retries
	// interleave backoff sleeps, so keep advancing until the run fails.
	for range 24 {
		clock.Advance(time.Minute)
		time.Sleep(2 * time.Millisecond)
		if run, err := disk.Run(ctx, summary.ID); err == nil && run.Status == RunFailed {
			break
		}
	}
	waitForRunStatus(t, disk, summary.ID, RunFailed, 2*time.Second)
	steps, err := disk.Steps(ctx, summary.ID)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	if len(steps) != 1 || steps[0].ErrorCode != string(ErrorCodeStepTimeout) {
		t.Fatalf("steps = %+v, want one timed-out step", steps)
	}
	if steps[0].Attempt != 3 {
		t.Fatalf("attempt = %d, want 3 (initial plus two retries)", steps[0].Attempt)
	}
	close(block)
}

func TestInterpreterBoundsConcurrentSteps(t *testing.T) {
	ctx := context.Background()
	registry, err := buildTestRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	fake := durable.NewFake()
	disk := newTestStore(t)
	var inFlight, maxInFlight atomic.Int64
	agents := &countingRunner{
		onStart: func() {
			current := inFlight.Add(1)
			for {
				old := maxInFlight.Load()
				if current <= old || maxInFlight.CompareAndSwap(old, current) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			inFlight.Add(-1)
		},
	}
	interpreter := NewInterpreter(disk, fake, &Options{
		MaxConcurrentSteps: 2,
		AgentResolver:      registry,
		Agents:             agents,
	})
	steps := make([]StepSpec, 0, 6)
	for _, id := range []string{"s1", "s2", "s3", "s4", "s5", "s6"} {
		steps = append(steps, StepSpec{ID: id, Executor: ExecutorSpec{Kind: KindAgent, AgentID: ptr("main")}, Timeout: 5 * time.Second})
	}
	spec := &Spec{ID: "fan", Goal: "Fan", Version: 1, Source: SourceDefined, Steps: steps}
	if err := interpreter.Load(ctx, spec, ValidationDeps{Agents: registry}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	summary, err := interpreter.Start(ctx, "fan", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForRunStatus(t, disk, summary.ID, RunSucceeded, 5*time.Second)
	if got := maxInFlight.Load(); got > 2 {
		t.Fatalf("max concurrent steps = %d, want <= 2", got)
	}
}

func TestInterpreterWaitSuspendsAndResumes(t *testing.T) {
	ctx := context.Background()
	registry, err := buildTestRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	fake := durable.NewFake()
	disk := newTestStore(t)
	interpreter := NewInterpreter(disk, fake, &Options{
		MaxConcurrentSteps: 2,
		AgentResolver:      registry,
	})
	spec := &Spec{
		ID: "waitflow", Goal: "Wait", Version: 1, Source: SourceDefined,
		Steps: []StepSpec{
			{ID: "ci", Executor: ExecutorSpec{Kind: KindWait, Event: ptr("ci.completed")}, Timeout: 5 * time.Second},
			{ID: "done", DependsOn: []string{"ci"}, Executor: ExecutorSpec{Kind: KindApproval}, Timeout: 5 * time.Second},
		},
	}
	if err := interpreter.Load(ctx, spec, ValidationDeps{Agents: registry}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	summary, err := interpreter.Start(ctx, "waitflow", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForRunStatus(t, disk, summary.ID, RunSuspended, 2*time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := fake.Signal(ctx, durable.RunRef{Key: summary.DurableKey}, "wait.ci", []byte(`{"state":"passed"}`)); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	waitForRunStatus(t, disk, summary.ID, RunSuspended, 2*time.Second)
	for time.Now().Before(deadline) {
		if err := fake.Signal(ctx, durable.RunRef{Key: summary.DurableKey}, "approval.done", []byte(`{"decision":"approve"}`)); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	waitForRunStatus(t, disk, summary.ID, RunSucceeded, 2*time.Second)
}

func TestInterpreterCancelReachesCancelled(t *testing.T) {
	ctx := context.Background()
	registry, err := buildTestRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	fake := durable.NewFake()
	disk := newTestStore(t)
	agents := blockingRunner{}
	interpreter := NewInterpreter(disk, fake, &Options{
		MaxConcurrentSteps: 1,
		AgentResolver:      registry,
		Agents:             agents,
	})
	spec := &Spec{
		ID: "cancelflow", Goal: "Cancel", Version: 1, Source: SourceDefined,
		Steps: []StepSpec{
			{ID: "work", Executor: ExecutorSpec{Kind: KindAgent, AgentID: ptr("main")}, Timeout: time.Hour},
		},
	}
	if err := interpreter.Load(ctx, spec, ValidationDeps{Agents: registry}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	summary, err := interpreter.Start(ctx, "cancelflow", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := interpreter.Cancel(ctx, summary.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	waitForRunStatus(t, disk, summary.ID, RunCancelled, 2*time.Second)
}

func TestLoadRejectsInvalidSpecWithoutPersisting(t *testing.T) {
	ctx := context.Background()
	disk := newTestStore(t)
	fake := durable.NewFake()
	interpreter := NewInterpreter(disk, fake, &Options{})
	spec := validTestSpec()
	spec.Steps[0].Executor.AgentID = nil
	deps := testValidationDeps()
	if err := interpreter.Load(ctx, spec, deps); err == nil {
		t.Fatal("Load accepted an invalid spec")
	}
	if specs, _ := disk.Definitions(ctx); len(specs) != 0 {
		t.Fatalf("invalid spec persisted %d definitions", len(specs))
	}
}

func TestStartUnknownDefinitionFailsClosed(t *testing.T) {
	ctx := context.Background()
	disk := newTestStore(t)
	interpreter := NewInterpreter(disk, durable.NewFake(), &Options{})
	if _, err := interpreter.Start(ctx, "ghost", nil); err == nil {
		t.Fatal("Start accepted an unknown definition")
	} else if code, _ := CodeOf(err); code != ErrorCodeDefinitionNotFound {
		t.Fatalf("code = %s, want workflow_definition_not_found", code)
	}
}

func waitForRunStatus(t *testing.T, disk *Store, runID, status string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		run, err := disk.Run(context.Background(), runID)
		if err == nil && run.Status == status {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	run, err := disk.Run(context.Background(), runID)
	if err != nil {
		t.Fatalf("run %s never reached %s: %v", runID, status, err)
	}
	t.Fatalf("run %s status = %s, want %s within %s", runID, run.Status, status, within)
}

func ptr(value string) *string { return &value }

func strPtr(value string) *string { return &value }

// snapshot helpers kept single-threaded-safe for assertions.
func (f *fakeAgentRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeToolRunner) snapshotCalled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.called...)
}

type countingRunner struct{ onStart func() }

func (c *countingRunner) Run(ctx context.Context, definition *auraagent.Definition, input *ExecutionContext) (json.RawMessage, error) {
	c.onStart()
	return json.RawMessage(`{"ok":true}`), nil
}

type blockingRunner struct{}

func (blockingRunner) Run(ctx context.Context, definition *auraagent.Definition, input *ExecutionContext) (json.RawMessage, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestSaveDefinitionIsIdempotentByContent(t *testing.T) {
	ctx := context.Background()
	disk := newTestStore(t)
	spec := validTestSpec()
	if err := disk.SaveDefinition(ctx, spec); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if err := disk.SaveDefinition(ctx, spec); err != nil {
		t.Fatalf("identical re-save must be a no-op: %v", err)
	}
	spec.Goal = "Different"
	if err := disk.SaveDefinition(ctx, spec); err == nil {
		t.Fatal("same (id, version) with different content was accepted")
	}
}

func TestApprovalRejectionRoutesRunToFailure(t *testing.T) {
	ctx := context.Background()
	registry, err := buildTestRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	fake := durable.NewFake()
	disk := newTestStore(t)
	interpreter := NewInterpreter(disk, fake, &Options{MaxConcurrentSteps: 1, AgentResolver: registry})
	spec := &Spec{
		ID: "rejectflow", Goal: "Reject", Version: 1, Source: SourceDefined,
		Steps: []StepSpec{
			{ID: "gate", Executor: ExecutorSpec{Kind: KindApproval}, Timeout: 5 * time.Second},
		},
	}
	if err := interpreter.Load(ctx, spec, ValidationDeps{Agents: registry}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	summary, err := interpreter.Start(ctx, "rejectflow", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForRunStatus(t, disk, summary.ID, RunSuspended, 2*time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := fake.Signal(ctx, durable.RunRef{Key: summary.DurableKey}, "approval.gate", []byte(`{"decision":"reject"}`)); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	waitForRunStatus(t, disk, summary.ID, RunFailed, 2*time.Second)
	steps, err := disk.Steps(ctx, summary.ID)
	if err != nil || steps[0].Status != StepFailed {
		t.Fatalf("approval step = %+v (%v), want failed", steps, err)
	}
}
