package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
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
		select {
		case <-f.inFlight:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
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

func newTestDB(t *testing.T) *sql.DB {
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
	return db
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(newTestDB(t))
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
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	if len(steps) != 1 || steps[0].Status != StepFailed {
		t.Fatalf("approval step = %+v (%v), want failed", steps, err)
	}
	if steps[0].ErrorCode != string(ErrorCodeApprovalRejected) {
		t.Fatalf("error code = %q, want %s", steps[0].ErrorCode, ErrorCodeApprovalRejected)
	}
}

func TestInterpreterSignalStepTimeoutRetryConvergesOnSignal(t *testing.T) {
	registry, err := buildTestRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	for _, tc := range []struct {
		name   string
		kind   Kind
		signal string
	}{
		{name: "wait", kind: KindWait, signal: "wait.gate"},
		{name: "approval", kind: KindApproval, signal: "approval.gate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			clock := durable.NewManualClock(time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC))
			fake := durable.NewFake().WithClock(clock)
			disk := newTestStore(t)
			interpreter := NewInterpreter(disk, fake, &Options{MaxConcurrentSteps: 1, AgentResolver: registry})
			step := StepSpec{ID: "gate", Executor: ExecutorSpec{Kind: tc.kind}, Timeout: time.Minute, Retry: RetryPolicy{Attempts: 1, Backoff: time.Minute}}
			if tc.kind == KindWait {
				step.Executor.Event = ptr("ci.completed")
			}
			spec := &Spec{
				ID: "retry-" + tc.name, Goal: "Signal step retry", Version: 1, Source: SourceDefined,
				Steps: []StepSpec{step},
			}
			if err := interpreter.Load(ctx, spec, ValidationDeps{Agents: registry}); err != nil {
				t.Fatalf("Load: %v", err)
			}
			summary, err := interpreter.Start(ctx, spec.ID, nil)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			waitForRunStatus(t, disk, summary.ID, RunSuspended, 2*time.Second)
			// The first attempt times out with no signal in flight; the
			// retry must park cleanly and converge on the next delivery.
			deadline := time.Now().Add(2 * time.Second)
			retried := false
			for time.Now().Before(deadline) {
				clock.Advance(time.Minute)
				time.Sleep(2 * time.Millisecond)
				steps, err := disk.Steps(ctx, summary.ID)
				if err == nil && len(steps) == 1 && steps[0].Attempt >= 1 {
					retried = true
					break
				}
			}
			if !retried {
				t.Fatal("retry attempt never started after the timeout")
			}
			if err := fake.Signal(ctx, durable.RunRef{Key: summary.DurableKey}, tc.signal, []byte(`{"decision":"approve"}`)); err != nil {
				t.Fatalf("Signal on the retried step: %v", err)
			}
			waitForRunStatus(t, disk, summary.ID, RunSucceeded, 2*time.Second)
			cancelDone := make(chan error, 1)
			go func() { cancelDone <- interpreter.Cancel(ctx, summary.ID) }()
			select {
			case err := <-cancelDone:
				if err != nil {
					t.Fatalf("Cancel: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Cancel did not return after the retried signal step")
			}
		})
	}
}

func TestInterpreterTimeoutCancelsAttemptBeforeRetry(t *testing.T) {
	ctx := context.Background()
	registry, err := buildTestRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	clock := durable.NewManualClock(time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC))
	fake := durable.NewFake().WithClock(clock)
	disk := newTestStore(t)
	tracker := &attemptTracker{events: map[int]string{}}
	interpreter := NewInterpreter(disk, fake, &Options{
		MaxConcurrentSteps: 1,
		AgentResolver:      registry,
		Agents:             tracker,
	})
	spec := &Spec{
		ID: "attemptflow", Goal: "Attempts", Version: 1, Source: SourceDefined,
		Steps: []StepSpec{
			{ID: "work", Executor: ExecutorSpec{Kind: KindAgent, AgentID: ptr("main")}, Timeout: time.Minute, Retry: RetryPolicy{Attempts: 2, Backoff: time.Minute}},
		},
	}
	if err := interpreter.Load(ctx, spec, ValidationDeps{Agents: registry}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	summary, err := interpreter.Start(ctx, "attemptflow", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for range 24 {
		clock.Advance(time.Minute)
		time.Sleep(2 * time.Millisecond)
		if run, err := disk.Run(ctx, summary.ID); err == nil && run.Status == RunFailed {
			break
		}
	}
	waitForRunStatus(t, disk, summary.ID, RunFailed, 2*time.Second)
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.maxLive != 1 {
		t.Fatalf("max live attempts = %d, want 1", tracker.maxLive)
	}
	if len(tracker.started) != 3 {
		t.Fatalf("attempts = %d, want 3", len(tracker.started))
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if !strings.Contains(tracker.events[attempt], "cancelled") {
			t.Fatalf("attempt %d ended without observing cancellation: %q", attempt, tracker.events[attempt])
		}
	}
}

type attemptTracker struct {
	mu      sync.Mutex
	started []int
	events  map[int]string
	active  int
	maxLive int
}

func (t *attemptTracker) Run(ctx context.Context, definition *auraagent.Definition, input *ExecutionContext) (json.RawMessage, error) {
	t.mu.Lock()
	t.active++
	if t.active > t.maxLive {
		t.maxLive = t.active
	}
	attempt := len(t.started) + 1
	t.started = append(t.started, attempt)
	t.mu.Unlock()
	<-ctx.Done()
	t.mu.Lock()
	t.active--
	t.events[attempt] = "cancelled before next attempt"
	t.mu.Unlock()
	return nil, ctx.Err()
}

type fakeArtifactSink struct {
	mu     sync.Mutex
	digest string
	stored [][]byte
	err    error
}

func (s *fakeArtifactSink) Put(ctx context.Context, content []byte) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	s.mu.Lock()
	s.stored = append(s.stored, content)
	s.mu.Unlock()
	return s.digest, nil
}

func oversizedOutput(size int) json.RawMessage {
	return json.RawMessage(`{"blob":"` + strings.Repeat("x", size) + `"}`)
}

func waitForStepStatus(t *testing.T, disk *Store, runID, stepID, status string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		steps, err := disk.Steps(context.Background(), runID)
		if err == nil {
			for _, step := range steps {
				if step.StepID == stepID && step.Status == status {
					return
				}
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("step %s on run %s never reached %s within %s", stepID, runID, status, within)
}

func TestInterpreterSinksOversizedOutputThroughArtifactSink(t *testing.T) {
	ctx := context.Background()
	registry, err := buildTestRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	fake := durable.NewFake()
	disk := newTestStore(t)
	sink := &fakeArtifactSink{digest: "sha256-abc123"}
	agents := &fakeAgentRunner{output: oversizedOutput(70000)}
	interpreter := NewInterpreter(disk, fake, &Options{
		MaxConcurrentSteps: 2,
		AgentResolver:      registry,
		Agents:             agents,
		Artifacts:          sink,
	})
	spec := &Spec{
		ID: "sinkflow", Goal: "Sink", Version: 1, Source: SourceDefined,
		Steps: []StepSpec{
			{ID: "work", Executor: ExecutorSpec{Kind: KindAgent, AgentID: ptr("main")}, Timeout: 5 * time.Second},
		},
	}
	if err := interpreter.Load(ctx, spec, ValidationDeps{Agents: registry}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	summary, err := interpreter.Start(ctx, "sinkflow", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForRunStatus(t, disk, summary.ID, RunSucceeded, 2*time.Second)
	steps, err := disk.Steps(ctx, summary.ID)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	if len(steps) != 1 || steps[0].Status != StepSucceeded {
		t.Fatalf("steps = %+v, want one succeeded step", steps)
	}
	if steps[0].OutputArtifactDigest != "sha256-abc123" {
		t.Fatalf("output_artifact_digest = %q, want the sink digest", steps[0].OutputArtifactDigest)
	}
	if want := `{"artifact_digest":"sha256-abc123"}`; string(steps[0].Output) != want {
		t.Fatalf("inline output = %s, want %s", steps[0].Output, want)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.stored) != 1 || len(sink.stored[0]) != len(oversizedOutput(70000)) {
		t.Fatalf("sink stored %d payloads, want the full oversized output", len(sink.stored))
	}
}

func TestInterpreterWaitPayloadOverInlineLimitIsSunk(t *testing.T) {
	ctx := context.Background()
	registry, err := buildTestRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	fake := durable.NewFake()
	disk := newTestStore(t)
	sink := &fakeArtifactSink{digest: "sha256-wait"}
	interpreter := NewInterpreter(disk, fake, &Options{
		MaxConcurrentSteps: 2,
		AgentResolver:      registry,
		Artifacts:          sink,
	})
	spec := &Spec{
		ID: "waitbig", Goal: "Wait big", Version: 1, Source: SourceDefined,
		Steps: []StepSpec{
			{ID: "ci", Executor: ExecutorSpec{Kind: KindWait, Event: ptr("ci.completed")}, Timeout: 5 * time.Second},
		},
	}
	if err := interpreter.Load(ctx, spec, ValidationDeps{Agents: registry}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	summary, err := interpreter.Start(ctx, "waitbig", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForRunStatus(t, disk, summary.ID, RunSuspended, 2*time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := fake.Signal(ctx, durable.RunRef{Key: summary.DurableKey}, "wait.ci", oversizedOutput(70000)); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	waitForRunStatus(t, disk, summary.ID, RunSucceeded, 2*time.Second)
	steps, err := disk.Steps(ctx, summary.ID)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	if len(steps) != 1 || steps[0].Status != StepSucceeded || steps[0].OutputArtifactDigest != "sha256-wait" {
		t.Fatalf("steps = %+v, want one succeeded step with the sink digest", steps)
	}
}

func TestInterpreterOversizedOutputWithoutSinkFailsTheRun(t *testing.T) {
	ctx := context.Background()
	registry, err := buildTestRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	fake := durable.NewFake()
	disk := newTestStore(t)
	agents := &fakeAgentRunner{output: oversizedOutput(70000)}
	interpreter := NewInterpreter(disk, fake, &Options{
		MaxConcurrentSteps: 2,
		AgentResolver:      registry,
		Agents:             agents,
	})
	spec := &Spec{
		ID: "nosink", Goal: "No sink", Version: 1, Source: SourceDefined,
		Steps: []StepSpec{
			{ID: "work", Executor: ExecutorSpec{Kind: KindAgent, AgentID: ptr("main")}, Timeout: 5 * time.Second},
		},
	}
	if err := interpreter.Load(ctx, spec, ValidationDeps{Agents: registry}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	summary, err := interpreter.Start(ctx, "nosink", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForRunStatus(t, disk, summary.ID, RunFailed, 2*time.Second)
	steps, err := disk.Steps(ctx, summary.ID)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	if len(steps) != 1 || steps[0].Status != StepFailed || steps[0].ErrorCode != string(ErrorCodeStepFailed) {
		t.Fatalf("steps = %+v, want one failed step with %s", steps, ErrorCodeStepFailed)
	}
}

func TestInterpreterStepPersistFailureFailsTheRun(t *testing.T) {
	ctx := context.Background()
	registry, err := buildTestRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	fake := durable.NewFake()
	db := newTestDB(t)
	disk := NewStore(db)
	block := make(chan struct{})
	agents := &fakeAgentRunner{inFlight: block}
	interpreter := NewInterpreter(disk, fake, &Options{
		MaxConcurrentSteps: 1,
		AgentResolver:      registry,
		Agents:             agents,
	})
	spec := &Spec{
		ID: "persistfail", Goal: "Persist fail", Version: 1, Source: SourceDefined,
		Steps: []StepSpec{
			{ID: "work", Executor: ExecutorSpec{Kind: KindAgent, AgentID: ptr("main")}, Timeout: 5 * time.Second},
		},
	}
	if err := interpreter.Load(ctx, spec, ValidationDeps{Agents: registry}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	summary, err := interpreter.Start(ctx, "persistfail", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForStepStatus(t, disk, summary.ID, "work", StepRunning, 2*time.Second)
	if _, err := db.ExecContext(ctx, `DELETE FROM workflow_step_run WHERE run_id = ?`, summary.ID); err != nil {
		t.Fatalf("delete step row: %v", err)
	}
	close(block)
	waitForRunStatus(t, disk, summary.ID, RunFailed, 2*time.Second)
}

func TestInterpreterSuspendedRunStaysSuspendedWhenSiblingCompletes(t *testing.T) {
	ctx := context.Background()
	registry, err := buildTestRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	fake := durable.NewFake()
	disk := newTestStore(t)
	agents := &fakeAgentRunner{}
	interpreter := NewInterpreter(disk, fake, &Options{
		MaxConcurrentSteps: 2,
		AgentResolver:      registry,
		Agents:             agents,
	})
	spec := &Spec{
		ID: "siblingflow", Goal: "Sibling", Version: 1, Source: SourceDefined,
		Steps: []StepSpec{
			{ID: "gate", Executor: ExecutorSpec{Kind: KindApproval}, Timeout: 5 * time.Second},
			{ID: "work", Executor: ExecutorSpec{Kind: KindAgent, AgentID: ptr("main")}, Timeout: 5 * time.Second},
		},
	}
	if err := interpreter.Load(ctx, spec, ValidationDeps{Agents: registry}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	summary, err := interpreter.Start(ctx, "siblingflow", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForStepStatus(t, disk, summary.ID, "work", StepSucceeded, 2*time.Second)
	waitForRunStatus(t, disk, summary.ID, RunSuspended, 2*time.Second)
	run, err := disk.Run(ctx, summary.ID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.Status != RunSuspended {
		t.Fatalf("run status = %s, want suspended while the approval step awaits its signal", run.Status)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := fake.Signal(ctx, durable.RunRef{Key: summary.DurableKey}, "approval.gate", []byte(`{"decision":"approve"}`)); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	waitForRunStatus(t, disk, summary.ID, RunSucceeded, 2*time.Second)
}

func TestInterpreterSuspendedStepReleasesConcurrencySlot(t *testing.T) {
	ctx := context.Background()
	registry, err := buildTestRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	fake := durable.NewFake()
	disk := newTestStore(t)
	started := make(chan struct{}, 1)
	agents := &countingRunner{onStart: func() { started <- struct{}{} }}
	interpreter := NewInterpreter(disk, fake, &Options{
		MaxConcurrentSteps: 1,
		AgentResolver:      registry,
		Agents:             agents,
	})
	spec := &Spec{
		ID: "slotflow", Goal: "Suspension releases the slot", Version: 1, Source: SourceDefined,
		Steps: []StepSpec{
			{ID: "gate", Executor: ExecutorSpec{Kind: KindApproval}, Timeout: 5 * time.Second},
			{ID: "work", Executor: ExecutorSpec{Kind: KindAgent, AgentID: ptr("main")}, Timeout: 5 * time.Second},
		},
	}
	if err := interpreter.Load(ctx, spec, ValidationDeps{Agents: registry}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	summary, err := interpreter.Start(ctx, spec.ID, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForRunStatus(t, disk, summary.ID, RunSuspended, 2*time.Second)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("dependency-ready agent step never started while the approval step stayed suspended")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := fake.Signal(ctx, durable.RunRef{Key: summary.DurableKey}, "approval.gate", []byte(`{"decision":"approve"}`)); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	waitForRunStatus(t, disk, summary.ID, RunSucceeded, 2*time.Second)
	steps, err := disk.Steps(ctx, summary.ID)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	for _, step := range steps {
		if step.Status != StepSucceeded {
			t.Fatalf("step %s = %s, want succeeded", step.StepID, step.Status)
		}
	}
}

func TestRunsOrdersRunsByIdentity(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	disk := NewStore(db)
	spec := validTestSpec()
	if err := disk.SaveDefinition(ctx, spec); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}
	first, err := disk.CreateRun(ctx, spec, nil)
	if err != nil {
		t.Fatalf("first CreateRun: %v", err)
	}
	second, err := disk.CreateRun(ctx, spec, nil)
	if err != nil {
		t.Fatalf("second CreateRun: %v", err)
	}
	older, newer := first.ID, second.ID
	if older > newer {
		older, newer = newer, older
	}
	epoch := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	recent := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	if _, err := db.ExecContext(ctx, `UPDATE workflow_run SET created_at = ? WHERE id = ?`, recent, newer); err != nil {
		t.Fatalf("stamp newer run: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE workflow_run SET created_at = ? WHERE id = ?`, epoch, older); err != nil {
		t.Fatalf("stamp older run: %v", err)
	}
	runs, err := disk.Runs(ctx, "")
	if err != nil {
		t.Fatalf("Runs: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}
	if runs[0].ID != older || runs[1].ID != newer {
		t.Fatalf("order = [%s %s], want identity order [%s %s]", runs[0].ID, runs[1].ID, older, newer)
	}
}

func TestSaveDefinitionConcurrentConflict(t *testing.T) {
	db := newTestDB(t)
	disk := NewStore(db)
	ctx := t.Context()

	spec1 := &Spec{
		ID:      "race-def",
		Version: 1,
		Goal:    "First variant",
		Source:  SourceDefined,
		Steps: []StepSpec{
			{ID: "s1", Executor: ExecutorSpec{Kind: KindWait, Event: ptr("ev1")}, Timeout: time.Minute},
		},
	}
	spec2 := &Spec{
		ID:      "race-def",
		Version: 1,
		Goal:    "Second variant",
		Source:  SourceDefined,
		Steps: []StepSpec{
			{ID: "s2", Executor: ExecutorSpec{Kind: KindWait, Event: ptr("ev2")}, Timeout: time.Minute},
		},
	}

	var wg sync.WaitGroup
	wg.Add(2)
	var err1, err2 error
	start := make(chan struct{})

	go func() {
		defer wg.Done()
		<-start
		err1 = disk.SaveDefinition(ctx, spec1)
	}()

	go func() {
		defer wg.Done()
		<-start
		err2 = disk.SaveDefinition(ctx, spec2)
	}()

	close(start)
	wg.Wait()

	successCount := 0
	for _, err := range []error{err1, err2} {
		if err == nil {
			successCount++
			continue
		}
		code, ok := CodeOf(err)
		if !ok || code != ErrorCodeSpecInvalid {
			t.Fatalf("concurrent save error = %v (code %v, ok %v), want %s", err, code, ok, ErrorCodeSpecInvalid)
		}
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			t.Fatalf("concurrent save surfaced raw constraint error: %v", err)
		}
	}
	if successCount != 1 {
		t.Fatalf("expected exactly 1 success, got %d (err1=%v, err2=%v)", successCount, err1, err2)
	}
}

type testApprovalRequester struct {
	mu       sync.Mutex
	requests [][2]string
	err      error
}

func (r *testApprovalRequester) Request(ctx context.Context, runID, stepID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, [2]string{runID, stepID})
	return r.err
}

func TestApprovalRequesterPortFailsStepAndRun(t *testing.T) {
	ctx := context.Background()
	spec := &Spec{
		ID:      "approval-port-fail-flow",
		Goal:    "Test approval requester port failure",
		Version: 1,
		Source:  SourceDefined,
		Steps: []StepSpec{
			{ID: "gate", Executor: ExecutorSpec{Kind: KindApproval}, Timeout: 5 * time.Second},
		},
	}
	disk := newTestStore(t)
	fake := durable.NewFake()
	requester := &testApprovalRequester{err: errors.New("approval backend unavailable")}
	interpreter := NewInterpreter(disk, fake, &Options{
		Approvals: requester,
	})
	if err := interpreter.Load(ctx, spec, ValidationDeps{}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	summary, err := interpreter.Start(ctx, spec.ID, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForRunStatus(t, disk, summary.ID, RunFailed, 2*time.Second)
	steps, err := disk.Steps(ctx, summary.ID)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("steps count = %d, want 1", len(steps))
	}
	if steps[0].Status != StepFailed {
		t.Fatalf("step status = %s, want failed", steps[0].Status)
	}
	if steps[0].ErrorCode != string(ErrorCodeStepFailed) {
		t.Fatalf("step error code = %s, want %s", steps[0].ErrorCode, ErrorCodeStepFailed)
	}
}

func TestApprovalRequesterPortSucceedsOnSignal(t *testing.T) {
	ctx := context.Background()
	spec := &Spec{
		ID:      "approval-port-success-flow",
		Goal:    "Test approval requester port success",
		Version: 1,
		Source:  SourceDefined,
		Steps: []StepSpec{
			{ID: "gate", Executor: ExecutorSpec{Kind: KindApproval}, Timeout: 5 * time.Second},
		},
	}
	disk := newTestStore(t)
	fake := durable.NewFake()
	requester := &testApprovalRequester{}
	interpreter := NewInterpreter(disk, fake, &Options{
		Approvals: requester,
	})
	if err := interpreter.Load(ctx, spec, ValidationDeps{}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	summary, err := interpreter.Start(ctx, spec.ID, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForRunStatus(t, disk, summary.ID, RunSuspended, 2*time.Second)
	requester.mu.Lock()
	reqCount := len(requester.requests)
	var recorded [2]string
	if reqCount > 0 {
		recorded = requester.requests[0]
	}
	requester.mu.Unlock()
	if reqCount != 1 {
		t.Fatalf("requests count = %d, want 1", reqCount)
	}
	if recorded[0] != summary.ID || recorded[1] != "gate" {
		t.Fatalf("recorded request = %v, want [%s gate]", recorded, summary.ID)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := fake.Signal(ctx, durable.RunRef{Key: summary.DurableKey}, "approval.gate", []byte(`{"decision":"approve"}`)); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	waitForRunStatus(t, disk, summary.ID, RunSucceeded, 2*time.Second)
}
