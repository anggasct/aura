package workflow

import (
	"context"
	"encoding/json"

	auraagent "github.com/anggasct/aura/internal/agent"
)

type AgentRunner interface {
	Run(ctx context.Context, definition *auraagent.Definition, input *ExecutionContext) (json.RawMessage, error)
}

type ToolRunner interface {
	Invoke(ctx context.Context, toolID string, args json.RawMessage) (json.RawMessage, error)
}

type ArtifactSink interface {
	Put(ctx context.Context, content []byte) (string, error)
}

type ApprovalRequester interface {
	Request(ctx context.Context, runID, stepID string) error
}
