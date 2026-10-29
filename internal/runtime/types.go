package runtime

import (
	"context"
	"encoding/json"
	"iter"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/principal"
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
	PrincipalClass   principal.Class
	Origin           Origin
	Parts            []runtimeingress.InputPart
	ReplyContext     json.RawMessage
	UntrustedContext *UntrustedRecall
	RequireRecall    bool
	IdempotencyKey   string
	Deadline         time.Time
	Budget           Budget
	TraceParent      string
	AgentID          string
	Persona          *PersonaFingerprint
}

type PersonaFingerprint struct {
	AgentID     string `json:"agent_id"`
	SoulSHA256  string `json:"soul_sha256"`
	ToolsSHA256 string `json:"tools_sha256"`
	ModelRoute  string `json:"model_route"`
}

type Personas interface {
	Fingerprint(agentID string) (PersonaFingerprint, bool)
}

type acceptedPayload struct {
	Origin    string              `json:"origin"`
	Principal string              `json:"principal"`
	AgentID   string              `json:"agent_id,omitempty"`
	Persona   *PersonaFingerprint `json:"persona_fingerprint,omitempty"`
}

// AcceptedPayload is the one encoding of turn.accepted; the in-memory and durable admit paths must journal identical bytes for the same turn.
func AcceptedPayload(origin, principalID, agentID string, persona *PersonaFingerprint) ([]byte, error) {
	return json.Marshal(acceptedPayload{Origin: origin, Principal: principalID, AgentID: agentID, Persona: persona})
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

	ProfileEvidenceStart = "[untrusted profile context"
	ProfileEvidenceEnd   = "[/untrusted profile context]"
)

type AgentRuntime interface {
	Run(ctx context.Context, req *TurnRequest) iter.Seq2[store.RuntimeEvent, error]
}

type ToolBroker interface {
	Evaluate(ctx context.Context, req *approval.ToolRequest) (approval.PolicyDecision, error)
}
