package workflow

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
)

type recordingBroker struct {
	sinkBroker
	requests []*toolbroker.ToolRequest
}

func (b *recordingBroker) Execute(ctx context.Context, request *toolbroker.ToolRequest) (toolbroker.ToolResult, error) {
	b.requests = append(b.requests, request)
	return b.sinkBroker.Execute(ctx, request)
}

func operationToolSpec(toolID string) *Spec {
	return &Spec{ID: "wf_operation", Goal: "Name a tool", Version: 1, Source: SourceDefined, Steps: []StepSpec{
		{ID: "act", Executor: ExecutorSpec{Kind: KindTool, ToolID: &toolID, ToolArgs: json.RawMessage(`{}`)}, Timeout: time.Minute},
	}}
}

func invokeToolStep(t *testing.T, broker *recordingBroker, toolID string) error {
	t.Helper()
	registry, err := buildTestRegistry()
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewToolStepRunner(&ToolStepRunnerDeps{Broker: broker, Sessions: ownerRunSession{}, Registry: registry, Runs: newTestStore(t)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	ctx = WithStepCall(ctx, &StepCall{
		RunID: "run-origin", StepID: "act", DefinitionID: "origin", DefinitionVersion: 1, DefinitionDigest: "digest",
		Origin:   RunOrigin{PrincipalID: principal.OwnerID, PrincipalClass: string(principal.ClassOwner), AgentID: "main"},
		Approval: Approval{DefinitionID: "origin", DefinitionVersion: 1, Digest: "digest", Kind: ApprovalInstalled},
	})
	_, err = runner.Invoke(ctx, toolID, json.RawMessage(`{"path":"a"}`))
	return err
}

func TestWorkflowStepRefusesOperationTools(t *testing.T) {
	t.Run("the compiler refuses a step naming an operation tool", func(t *testing.T) {
		deps := testValidationDeps()
		deps.KnownTools = append(deps.KnownTools, "aura_cron_list")
		err := Validate(operationToolSpec("aura_cron_list"), deps)
		if !hasCode(err, ErrorCodeSpecInvalid) {
			t.Fatalf("Validate = %v, want %s", err, ErrorCodeSpecInvalid)
		}
	})
	t.Run("the runner refuses a registered operation tool before the broker", func(t *testing.T) {
		broker := &recordingBroker{sinkBroker: sinkBroker{definitions: map[string]tools.Definition{
			"aura_cron_list": {Name: "aura_cron_list", Version: tools.Version, Operation: "cron.list"},
		}}}
		err := invokeToolStep(t, broker, "aura_cron_list")
		if !hasCode(err, ErrorCodeCapabilityExceeded) {
			t.Fatalf("Invoke = %v, want %s", err, ErrorCodeCapabilityExceeded)
		}
		if len(broker.requests) != 0 {
			t.Fatalf("the broker ran %d requests, want none", len(broker.requests))
		}
	})
}

func TestToolStepRequestsCarryTheWorkflowOrigin(t *testing.T) {
	broker := &recordingBroker{sinkBroker: sinkBroker{definitions: map[string]tools.Definition{
		"read_file": {Name: "read_file", Version: tools.Version},
	}}}
	if err := invokeToolStep(t, broker, "read_file"); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(broker.requests) != 1 || broker.requests[0].Origin != turn.OriginWorkflow {
		t.Fatalf("requests = %+v, want one with the workflow origin", broker.requests)
	}
}
