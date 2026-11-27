package workflow

import (
	"context"
	"encoding/json"

	"github.com/anggasct/aura/internal/platform/durable"
)

type AgentStepCall struct {
	RunID             string
	StepID            string
	Attempt           int
	Origin            RunOrigin
	DefinitionID      string
	DefinitionVersion int
	DefinitionDigest  string
	Executor          ExecutorSpec
	Input             RunInput
	Invocation        durable.Invocation
}

type AgentStepResult struct {
	Output  json.RawMessage
	Tokens  int64
	Tainted bool
	Private bool
}

type AgentStepRunner interface {
	RunStep(ctx context.Context, call *AgentStepCall) (AgentStepResult, error)
}

type ToolStepResult struct {
	Output  json.RawMessage
	Tainted bool
}

type ToolRunner interface {
	Invoke(ctx context.Context, toolID string, args json.RawMessage) (ToolStepResult, error)
}

type ArtifactSink interface {
	Put(ctx context.Context, content []byte) (string, error)
}

type ApprovalRequester interface {
	Request(ctx context.Context, runID, stepID string) error
}
