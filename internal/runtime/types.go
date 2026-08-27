package runtime

import (
	"context"
	"iter"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/runtime/ingress"
	"github.com/anggasct/aura/internal/store"
)

// Origin identifies where a turn entered the system. Adapters that bridge an
// external channel set it to that channel; a stable string, not a URL or
// free-form description, so it can be used in dedupe keys and metrics.
type Origin string

const (
	OriginTerminal Origin = "terminal"
	OriginInternal Origin = "internal"
)

// Budget bounds a turn's consumption. Zero fields mean "no explicit limit
// beyond the runtime defaults"; enforcement lives in the usage ledger.
type Budget struct {
	MaxTokens int64
	MaxCost   float64
}

// TurnRequest is the normalized, identity-checked request every trigger
// submits through AgentRuntime. Channel adapters build it from their
// IngressEnvelope; jobs and subagents build it directly.
type TurnRequest struct {
	TurnID         string
	SessionID      string
	PrincipalID    string
	Origin         Origin
	Parts          []runtimeingress.InputPart
	IdempotencyKey string
	Deadline       time.Time
	Budget         Budget
	TraceParent    string
}

// Stable runtime event kinds. Consumers ignore unknown additive kinds.
const (
	EventKindTurnAccepted     = "turn.accepted"
	EventKindModelStarted     = "model.started"
	EventKindModelDelta       = "model.delta"
	EventKindToolRequested    = "tool.requested"
	EventKindApprovalRequired = "approval.required"
	EventKindToolStarted      = "tool.started"
	EventKindToolCompleted    = "tool.completed"
	EventKindMessageCompleted = "message.completed"
	EventKindTurnCompleted    = "turn.completed"
	EventKindTurnFailed       = "turn.failed"
	EventKindTurnCancelled    = "turn.cancelled"
)

// AgentRuntime is the single boundary every trigger uses to run work.
// Gateways, schedulers, webhooks, and subagents must not build their own
// turn loop. The stream carries the persisted event type, so what is
// streamed is exactly what is durable.
//
// The request is passed by pointer per code conventions (heavy structs are
// pointer parameters); implementations must treat the request as read-only
// and copy before mutating.
type AgentRuntime interface {
	Run(ctx context.Context, req *TurnRequest) iter.Seq2[store.RuntimeEvent, error]
}

// ToolBroker is the policy gate every tool call must pass before execution.
// approval.Engine is the canonical implementation.
type ToolBroker interface {
	Evaluate(ctx context.Context, req *approval.ToolRequest) (approval.PolicyDecision, error)
}
