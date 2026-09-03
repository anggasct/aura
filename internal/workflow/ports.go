package workflow

import (
	"context"
	"encoding/json"

	auraagent "github.com/anggasct/aura/internal/agent"
)

// AgentRunner runs one bounded agent execution for an agent step; the
// composition root supplies the production implementation and tests a fake.
type AgentRunner interface {
	Run(ctx context.Context, definition *auraagent.Definition, input *ExecutionContext) (json.RawMessage, error)
}

// ToolRunner invokes one tool through the broker, unchanged.
type ToolRunner interface {
	Invoke(ctx context.Context, toolID string, args json.RawMessage) (json.RawMessage, error)
}

// ArtifactSink stores oversized step outputs content-addressed and returns
// the digest.
type ArtifactSink interface {
	Put(ctx context.Context, content []byte) (string, error)
}

// ApprovalRequester binds one approval request to a run+step using the
// existing approval policy surface. The port is reserved for the production
// approval surface.
type ApprovalRequester interface {
	Request(ctx context.Context, runID, stepID string) error
}
