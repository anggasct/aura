package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/durable/durabletest"
)

type labelProbe struct {
	mu       sync.Mutex
	reported map[string]bool
	requests map[string]bool
	calls    []string
}

func (p *labelProbe) observe(stepID string, requestTainted bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, stepID)
	p.requests[stepID] = requestTainted
	return p.reported[stepID]
}

func (p *labelProbe) RunStep(_ context.Context, call *AgentStepCall) (AgentStepResult, error) {
	tainted := p.observe(call.StepID, call.Origin.Tainted)
	return AgentStepResult{Output: json.RawMessage(`{"ok":true}`), Tainted: tainted}, nil
}

func (p *labelProbe) Invoke(ctx context.Context, _ string, _ json.RawMessage) (ToolStepResult, error) {
	call, _ := StepCallFrom(ctx)
	tainted := p.observe(call.StepID, call.Origin.Tainted)
	return ToolStepResult{Output: json.RawMessage(`{"ok":true}`), Tainted: tainted}, nil
}

func (p *labelProbe) reset(reported map[string]bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reported = reported
	p.requests = map[string]bool{}
	p.calls = nil
}

func (p *labelProbe) snapshot() (requests map[string]bool, calls []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return maps.Clone(p.requests), slices.Sorted(slices.Values(p.calls))
}

type journalWatch struct {
	durable.Invocation
	mu      sync.Mutex
	records map[string][]byte
	live    map[string]bool
}

func (w *journalWatch) RunAction(ctx context.Context, key string, fn func(ctx context.Context) ([]byte, error)) ([]byte, error) {
	inner := w.Invocation.RunAction
	w.mu.Lock()
	if w.live[key] {
		inner = func(ctx context.Context, _ string, fn func(ctx context.Context) ([]byte, error)) ([]byte, error) {
			return fn(ctx)
		}
	}
	w.mu.Unlock()
	raw, err := inner(ctx, key, fn)
	if err == nil {
		w.mu.Lock()
		w.records[key] = raw
		w.mu.Unlock()
	}
	return raw, err
}

func (w *journalWatch) attempt(t *testing.T, runID, stepID string) journaledAttempt {
	t.Helper()
	w.mu.Lock()
	raw, ok := w.records["run/"+runID+"/step/"+stepID+"/attempt/0"]
	w.mu.Unlock()
	if !ok {
		t.Fatalf("step %s has no journaled attempt", stepID)
	}
	var record journaledAttempt
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("decode journaled attempt of %s: %v", stepID, err)
	}
	return record
}

func labelStep(id string, executor ExecutorSpec, dependsOn ...string) StepSpec {
	return StepSpec{ID: id, DependsOn: dependsOn, Executor: executor, Timeout: 5 * time.Second}
}

func webAgent() ExecutorSpec {
	executor := agentExecutor("engineer")
	executor.Tools = []string{"web_search"}
	return executor
}

func workspaceAgent() ExecutorSpec {
	executor := agentExecutor("engineer")
	executor.Tools = []string{"read_file"}
	return executor
}

func toolExecutor(toolID string) ExecutorSpec {
	return ExecutorSpec{Kind: KindTool, ToolID: &toolID, ToolArgs: json.RawMessage(`{"path":"a"}`)}
}

func taintTestSetup(t *testing.T, probe *labelProbe) (*Interpreter, *durabletest.Fake, *Store) {
	t.Helper()
	fake := durabletest.NewFake()
	disk := newTestStore(t)
	interpreter := mustNewInterpreter(t, disk, fake, &Options{MaxConcurrentSteps: 2, AgentSteps: probe, Tools: probe, Driver: DriverSerial})
	return interpreter, fake, disk
}

func taintTestDeps() *ValidationDeps {
	deps := testValidationDeps()
	deps.KnownTools = append(deps.KnownTools, "web_fetch")
	return deps
}

func TestWorkflowStepTaint(t *testing.T) {
	leaf := toolSpec("leaf")
	cases := []struct {
		name         string
		origin       func() *RunOrigin
		approval     bool
		steps        []StepSpec
		reported     map[string]bool
		wantRequest  map[string]bool
		wantOutput   map[string]bool
		wantPrivate  map[string]bool
		replayed     []string
		wantChildren []bool
		childCalls   []string
	}{
		{
			name:        "a web result taints the output of an agent step in an untainted run",
			steps:       []StepSpec{labelStep("research", webAgent())},
			reported:    map[string]bool{"research": true},
			wantRequest: map[string]bool{"research": false},
			wantOutput:  map[string]bool{"research": true},
			replayed:    []string{"research"},
		},
		{
			name:        "a tainted origin taints the request and the output of an agent step",
			origin:      func() *RunOrigin { origin := ownerOrigin(); origin.Tainted = true; return origin },
			steps:       []StepSpec{labelStep("summarize", agentExecutor("engineer"))},
			wantRequest: map[string]bool{"summarize": true},
			wantOutput:  map[string]bool{"summarize": true},
			replayed:    []string{"summarize"},
		},
		{
			name:        "a tainted approval taints the request and the output of an agent step",
			approval:    true,
			steps:       []StepSpec{labelStep("summarize", agentExecutor("engineer"))},
			wantRequest: map[string]bool{"summarize": true},
			wantOutput:  map[string]bool{"summarize": true},
			replayed:    []string{"summarize"},
		},
		{
			name: "an agent step reading a tainted dependency runs tainted",
			steps: []StepSpec{
				labelStep("research", webAgent()),
				labelStep("digest", agentExecutor("engineer"), "research"),
			},
			reported:    map[string]bool{"research": true},
			wantRequest: map[string]bool{"research": false, "digest": true},
			wantOutput:  map[string]bool{"research": true, "digest": true},
			replayed:    []string{"digest"},
		},
		{
			name: "a tool step after a tainted agent step with no condition stays untainted",
			steps: []StepSpec{
				labelStep("research", webAgent()),
				labelStep("record", toolExecutor("read_file"), "research"),
			},
			reported:    map[string]bool{"research": true},
			wantRequest: map[string]bool{"research": false, "record": false},
			wantOutput:  map[string]bool{"research": true, "record": false},
			replayed:    []string{"record"},
		},
		{
			name: "a condition on a tainted output steers a tool step and its transitive dependents",
			steps: []StepSpec{
				labelStep("research", webAgent()),
				withCondition(labelStep("gate", toolExecutor("read_file"), "research"), "steps.research.output.ok == true"),
				labelStep("later", toolExecutor("read_file"), "gate"),
				labelStep("plain", toolExecutor("read_file"), "research"),
			},
			reported:    map[string]bool{"research": true},
			wantRequest: map[string]bool{"research": false, "gate": true, "later": true, "plain": false},
			replayed:    []string{"gate", "later", "plain"},
		},
		{
			name: "a condition on the status of an unsteered step with a tainted output does not steer",
			steps: []StepSpec{
				labelStep("research", webAgent()),
				withCondition(labelStep("check", toolExecutor("read_file"), "research"), `steps.research.status == "succeeded"`),
			},
			reported:    map[string]bool{"research": true},
			wantRequest: map[string]bool{"research": false, "check": false},
			replayed:    []string{"check"},
		},
		{
			name: "a condition on the status of a steered step steers",
			steps: []StepSpec{
				labelStep("research", webAgent()),
				labelStep("mid", agentExecutor("engineer"), "research"),
				withCondition(labelStep("probe", toolExecutor("read_file"), "research"), `steps.mid.status == "succeeded"`),
			},
			reported:    map[string]bool{"research": true},
			wantRequest: map[string]bool{"research": false, "mid": true, "probe": true},
			replayed:    []string{"probe"},
		},
		{
			name: "a steered workflow step starts a tainted child run",
			steps: []StepSpec{
				labelStep("research", webAgent()),
				withCondition(labelStep("steered", ExecutorSpec{Kind: KindWorkflow, WorkflowID: ptr("leaf")}, "research"), "steps.research.output.ok == true"),
			},
			reported:     map[string]bool{"research": true},
			wantRequest:  map[string]bool{"research": false},
			replayed:     []string{"steered"},
			wantChildren: []bool{true},
			childCalls:   []string{"read"},
		},
		{
			name: "an unsteered workflow step starts an untainted child run",
			steps: []StepSpec{
				labelStep("research", webAgent()),
				labelStep("plain", ExecutorSpec{Kind: KindWorkflow, WorkflowID: ptr("leaf")}, "research"),
			},
			reported:     map[string]bool{"research": true},
			wantRequest:  map[string]bool{"research": false},
			replayed:     []string{"plain"},
			wantChildren: []bool{false},
			childCalls:   []string{"read"},
		},
		{
			name: "the output of a web fetch tool step is tainted and steers a condition",
			steps: []StepSpec{
				labelStep("fetch", toolExecutor("web_fetch")),
				withCondition(labelStep("use", toolExecutor("read_file"), "fetch"), "steps.fetch.output.ok == true"),
			},
			reported:    map[string]bool{"fetch": true},
			wantRequest: map[string]bool{"fetch": false, "use": true},
			wantOutput:  map[string]bool{"fetch": true, "use": false},
			replayed:    []string{"use"},
		},
		{
			name: "private labels follow tool steps, workspace agent steps and their readers",
			steps: []StepSpec{
				labelStep("research", webAgent()),
				labelStep("read", workspaceAgent()),
				labelStep("summarize", agentExecutor("engineer"), "read"),
				labelStep("record", toolExecutor("read_file")),
				labelStep("fetch", toolExecutor("web_fetch")),
			},
			wantPrivate: map[string]bool{"research": false, "read": true, "summarize": true, "record": true, "fetch": false},
			replayed:    []string{"summarize"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			probe := &labelProbe{}
			probe.reset(tc.reported)
			interpreter, fake, disk := taintTestSetup(t, probe)
			spec := &Spec{ID: "labels", Goal: "Labels", Version: 1, Source: SourceDefined, Steps: tc.steps}
			if err := interpreter.Install(ctx, []*Spec{leaf, spec}, taintTestDeps()); err != nil {
				t.Fatalf("Install: %v", err)
			}
			origin := ownerOrigin()
			if tc.origin != nil {
				origin = tc.origin()
			}
			var summary *RunSummary
			if tc.approval {
				holdWorkflowHandler(t, fake)
				summary = runStartedUnderATaintedApproval(t, interpreter, disk, spec)
			} else {
				started, err := disk.newRun(ctx, spec, &RunInput{}, origin)
				if err != nil {
					t.Fatalf("newRun: %v", err)
				}
				summary = started
			}
			inv, _ := holdInvocation(t, fake, "holder-labels")
			watch := &journalWatch{Invocation: inv, records: map[string][]byte{}, live: map[string]bool{}}

			if terminal, err := interpreter.DriveSerial(ctx, watch, summary.ID); err != nil || terminal != RunSucceeded {
				t.Fatalf("DriveSerial = %s, %v; want %s", terminal, err, RunSucceeded)
			}
			requests, _ := probe.snapshot()
			for stepID, want := range tc.wantRequest {
				if got, ok := requests[stepID]; !ok || got != want {
					t.Errorf("request taint of %s = %v (ran %v), want %v", stepID, got, ok, want)
				}
			}
			for stepID, want := range tc.wantOutput {
				if got := watch.attempt(t, summary.ID, stepID).Tainted; got != want {
					t.Errorf("journaled output taint of %s = %v, want %v", stepID, got, want)
				}
			}
			for stepID, want := range tc.wantPrivate {
				if got := watch.attempt(t, summary.ID, stepID).Private; got != want {
					t.Errorf("journaled private label of %s = %v, want %v", stepID, got, want)
				}
			}
			if tc.wantChildren != nil {
				assertChildTaint(t, disk, summary.ID, tc.wantChildren)
			}

			flipped := map[string]bool{}
			for stepID := range tc.reported {
				flipped[stepID] = !tc.reported[stepID]
			}
			for _, step := range tc.steps {
				if !slices.Contains(tc.replayed, step.ID) {
					flipped[step.ID] = !tc.reported[step.ID]
					continue
				}
				flipped[step.ID] = tc.reported[step.ID]
				watch.live["run/"+summary.ID+"/step/"+step.ID+"/attempt/0"] = true
				watch.live["run/"+summary.ID+"/step/"+step.ID+"/attempt/0/child"] = true
			}
			probe.reset(flipped)
			if terminal, err := interpreter.DriveSerial(ctx, watch, summary.ID); err != nil || terminal != RunSucceeded {
				t.Fatalf("replayed DriveSerial = %s, %v; want %s", terminal, err, RunSucceeded)
			}
			replayedRequests, calls := probe.snapshot()
			want := slices.Clone(tc.childCalls)
			for _, step := range tc.steps {
				if slices.Contains(tc.replayed, step.ID) && step.Executor.Kind != KindWorkflow {
					want = append(want, step.ID)
				}
			}
			slices.Sort(want)
			if !slices.Equal(calls, want) {
				t.Fatalf("steps run again on the replayed drive = %v, want only %v", calls, want)
			}
			for _, stepID := range tc.replayed {
				if want, ok := tc.wantRequest[stepID]; ok && replayedRequests[stepID] != want {
					t.Errorf("replayed request taint of %s = %v, want %v", stepID, replayedRequests[stepID], want)
				}
			}
			for stepID, want := range tc.wantOutput {
				if got := watch.attempt(t, summary.ID, stepID).Tainted; got != want {
					t.Errorf("replayed output taint of %s = %v, want %v", stepID, got, want)
				}
			}
			if tc.wantChildren != nil {
				assertChildTaint(t, disk, summary.ID, append(slices.Clone(tc.wantChildren), tc.wantChildren...))
			}
		})
	}
}

func holdWorkflowHandler(t *testing.T, fake *durabletest.Fake) {
	t.Helper()
	release := make(chan struct{})
	fake.RegisterHandler(HandlerName, func(ctx context.Context, _ durable.Invocation) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})
	t.Cleanup(func() { close(release) })
}

func runStartedUnderATaintedApproval(t *testing.T, interpreter *Interpreter, disk *Store, spec *Spec) *RunSummary {
	t.Helper()
	generated := NormalizeProposal(spec, "prop_taint")
	if err := interpreter.LoadAll(t.Context(), []*Spec{generated}, taintTestDeps()); err != nil {
		t.Fatal(err)
	}
	proposal := proposeSpec(t, disk, "prop_taint", "ops", true, generated)
	if err := disk.ApproveProposal(t.Context(), proposal.ID, proposalApproval(proposal)); err != nil {
		t.Fatal(err)
	}
	run, err := interpreter.StartApproved(t.Context(), proposal.ID, &RunInput{}, ownerOrigin())
	if err != nil {
		t.Fatal(err)
	}
	if !run.Origin.Tainted {
		t.Fatal("a run started under a tainted approval is not tainted")
	}
	return run
}

func assertChildTaint(t *testing.T, disk *Store, parentID string, want []bool) {
	t.Helper()
	rows, err := disk.db.QueryContext(t.Context(), `SELECT tainted FROM workflow_run WHERE parent_run_id = ? ORDER BY created_at, id`, parentID)
	if err != nil {
		t.Fatalf("read child runs: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var got []bool
	for rows.Next() {
		var tainted bool
		if err := rows.Scan(&tainted); err != nil {
			t.Fatalf("scan child run: %v", err)
		}
		got = append(got, tainted)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read child runs: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("child run taints = %v, want %v", got, want)
	}
}

type codedAgentRunner struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (r *codedAgentRunner) RunStep(context.Context, *AgentStepCall) (AgentStepResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return AgentStepResult{}, r.err
}

func TestAgentStepFailureCodesFinalWithoutRetry(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantCode  ErrorCode
		wantCalls int
	}{
		{name: "a persona that no longer admits the class", err: codedError(ErrorCodeNotPermitted, "denied"), wantCode: ErrorCodeNotPermitted, wantCalls: 1},
		{name: "an exhausted budget", err: codedError(ErrorCodeBudgetExhausted, "spent"), wantCode: ErrorCodeBudgetExhausted, wantCalls: 1},
		{name: "a wrapped final code", err: errors.Join(errors.New("context"), codedError(ErrorCodeNotPermitted, "denied")), wantCode: ErrorCodeNotPermitted, wantCalls: 1},
		{name: "a missing capability is retried", err: codedError(ErrorCodeCapabilityUnavailable, "no provider"), wantCode: ErrorCodeCapabilityUnavailable, wantCalls: 3},
		{name: "an uncoded error is retried", err: errors.New("boom"), wantCode: ErrorCodeStepFailed, wantCalls: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			runner := &codedAgentRunner{err: tc.err}
			fake := durabletest.NewFake()
			disk := newTestStore(t)
			interpreter := mustNewInterpreter(t, disk, fake, &Options{MaxConcurrentSteps: 2, AgentSteps: runner, Driver: DriverSerial})
			step := labelStep("think", agentExecutor("engineer"))
			step.Retry = RetryPolicy{Attempts: 2}
			runID := startSerialRun(t, interpreter, disk, &Spec{ID: "final", Goal: "Final", Version: 1, Source: SourceDefined, Steps: []StepSpec{step}}, &RunInput{})
			inv, _ := holdInvocation(t, fake, "holder-final")
			if terminal, err := interpreter.DriveSerial(ctx, inv, runID); err != nil || terminal != RunFailed {
				t.Fatalf("DriveSerial = %s, %v; want %s", terminal, err, RunFailed)
			}
			steps, err := disk.Steps(ctx, runID)
			if err != nil || len(steps) != 1 {
				t.Fatalf("Steps = %d, %v; want one", len(steps), err)
			}
			if steps[0].ErrorCode != string(tc.wantCode) || runner.calls != tc.wantCalls {
				t.Fatalf("step code = %s after %d calls, want %s after %d", steps[0].ErrorCode, runner.calls, tc.wantCode, tc.wantCalls)
			}
		})
	}
}
