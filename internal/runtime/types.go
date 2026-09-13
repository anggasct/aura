package runtime

import (
	"context"
	"iter"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/runtime/ingress"
	"github.com/anggasct/aura/internal/store"
)

type Origin string

const (
	OriginTerminal Origin = "terminal"
	OriginInternal Origin = "internal"
	OriginWebhook  Origin = "webhook"
)

type Budget struct {
	MaxTokens int64
	MaxCost   float64
}

type TurnRequest struct {
	TurnID           string
	SessionID        string
	PrincipalID      string
	Origin           Origin
	Parts            []runtimeingress.InputPart
	UntrustedContext *UntrustedRecall
	RequireRecall    bool
	IdempotencyKey   string
	Deadline         time.Time
	Budget           Budget
	TraceParent      string
	AgentID          string
}

type UntrustedDocument struct {
	ID           string
	SessionID    string
	FromSequence uint64
	ToSequence   uint64
	Trust        approval.TrustLabel
	Content      string
}

type UntrustedRecall struct {
	Query     string
	Summary   string
	Trust     approval.TrustLabel
	Documents []UntrustedDocument
}

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

const (
	RecallEvidenceStart = "[untrusted recall evidence"
	RecallEvidenceEnd   = "[/untrusted recall evidence]"
)

type AgentRuntime interface {
	Run(ctx context.Context, req *TurnRequest) iter.Seq2[store.RuntimeEvent, error]
}

type ToolBroker interface {
	Evaluate(ctx context.Context, req *approval.ToolRequest) (approval.PolicyDecision, error)
}
