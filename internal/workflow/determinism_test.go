package workflow

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/durable"
)

func runScriptedWorkflow(t *testing.T, script []struct {
	name    string
	payload string
}) map[string]string {
	t.Helper()
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
		Agents:             &fakeAgentRunner{output: json.RawMessage(`{"decision":"approve"}`)},
		Tools: &fakeToolRunner{output: func(toolID string) json.RawMessage {
			return json.RawMessage(`{"pr":123}`)
		}},
	})
	tool := "read_file"
	spec := &Spec{
		ID: "replay", Goal: "Replay", Version: 1, Source: SourceDefined,
		Steps: []StepSpec{
			{ID: "implement", Executor: ExecutorSpec{Kind: KindAgent, AgentID: ptr("engineer")}, Timeout: 5 * time.Second},
			{ID: "verify", DependsOn: []string{"implement"}, Executor: ExecutorSpec{Kind: KindAgent, RequiredCapabilities: []string{auraagent.CapabilityRepositoryRead}}, Timeout: 5 * time.Second},
			{ID: "record", DependsOn: []string{"verify"}, Executor: ExecutorSpec{Kind: KindTool, ToolID: &tool}, Timeout: 5 * time.Second},
			{ID: "approve", DependsOn: []string{"record"}, Executor: ExecutorSpec{Kind: KindApproval}, Timeout: 5 * time.Second},
		},
	}
	if err := interpreter.Load(ctx, spec, testValidationDeps()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	summary, err := interpreter.Start(ctx, "replay", &RunInput{Objective: "ship it"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, signal := range script {
		waitForRunStatus(t, disk, summary.ID, RunSuspended, 2*time.Second)
		if err := interpreter.Signal(ctx, summary.ID, signal.name, []byte(signal.payload)); err != nil {
			t.Fatalf("signal %s: %v", signal.name, err)
		}
	}
	waitForRunStatus(t, disk, summary.ID, RunSucceeded, 2*time.Second)
	steps, err := disk.Steps(ctx, summary.ID)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	outcomes := map[string]string{}
	for _, step := range steps {
		outcomes[step.StepID] = step.Status + ":" + string(step.Output)
	}
	return outcomes
}

func TestInterpreterRepeatedScriptReproducesStepOutcomes(t *testing.T) {
	script := []struct {
		name    string
		payload string
	}{
		{"approval.approve", `{"decision":"approve"}`},
	}
	first := runScriptedWorkflow(t, script)
	second := runScriptedWorkflow(t, script)
	if len(first) != len(second) {
		t.Fatalf("outcome counts differ: %d vs %d", len(first), len(second))
	}
	for stepID, want := range first {
		if second[stepID] != want {
			t.Errorf("step %s outcome differs across replays:\nfirst:  %s\nsecond: %s", stepID, want, second[stepID])
		}
	}
}
