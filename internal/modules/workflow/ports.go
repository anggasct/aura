package workflow

import (
	"context"
	"encoding/json"
	"iter"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
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
	Inputs            []AgentStepInput
	RunTokensLeft     int64
	Invocation        durable.Invocation
}

type AgentStepInput struct {
	StepID  string
	Output  json.RawMessage
	Private bool
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

type StepTurns interface {
	Execute(ctx context.Context, req *turn.TurnRequest) iter.Seq2[turn.Event, error]
}

type StepEvents interface {
	UpsertEvent(ctx context.Context, event *turn.Event) (sequence uint64, existed bool, err error)
}

type StepToolCatalog interface {
	Definition(name, version string) (tools.Definition, bool)
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
