package runtimesessions

import (
	"encoding/json"
	"time"

	"github.com/anggasct/aura/internal/platform/turn"
)

const MaxImageEnvelopeBytes = 1 << 20

type Part struct {
	Text  string          `json:"text"`
	Image json.RawMessage `json:"image,omitempty"`
}

type UntrustedDocument struct {
	ID           string `json:"id"`
	SessionID    string `json:"session_id"`
	FromSequence uint64 `json:"from_sequence"`
	ToSequence   uint64 `json:"to_sequence"`
	Trust        string `json:"trust"`
	Content      string `json:"content"`
}

type UntrustedContext struct {
	Query     string              `json:"query"`
	Summary   string              `json:"summary"`
	Trust     string              `json:"trust"`
	Documents []UntrustedDocument `json:"documents,omitempty"`
}

type Descriptor struct {
	TurnID           string                   `json:"turn_id"`
	SessionID        string                   `json:"session_id"`
	PrincipalID      string                   `json:"principal_id"`
	PrincipalClass   string                   `json:"principal_class,omitempty"`
	Origin           string                   `json:"origin"`
	Parts            []Part                   `json:"parts"`
	ReplyContext     json.RawMessage          `json:"reply_context,omitempty"`
	UntrustedContext *UntrustedContext        `json:"untrusted_context,omitempty"`
	RequireRecall    bool                     `json:"require_recall,omitempty"`
	IdempotencyKey   string                   `json:"idempotency_key"`
	Deadline         time.Time                `json:"deadline,omitempty"`
	MaxTokens        int64                    `json:"max_tokens"`
	MaxCost          float64                  `json:"max_cost"`
	TraceParent      string                   `json:"trace_parent"`
	AgentID          string                   `json:"agent_id"`
	Persona          *turn.PersonaFingerprint `json:"persona_fingerprint,omitempty"`
	Skill            *turn.SkillPin           `json:"skill,omitempty"`
	StreamPartial    bool                     `json:"stream_partial,omitempty"`
	Knowledge        []string                 `json:"knowledge,omitempty"`
}

type QueuedTurn struct {
	Descriptor Descriptor `json:"descriptor"`
	Sequence   uint64     `json:"sequence"`
}

type ActiveTurn struct {
	Descriptor Descriptor `json:"descriptor"`
	Sequence   uint64     `json:"sequence"`
}

func (d *Descriptor) Validate() error {
	if d == nil {
		return invalidArgument("turn descriptor must not be nil")
	}
	if d.TurnID == "" {
		return invalidArgument("turn id must not be empty")
	}
	if d.SessionID == "" {
		return invalidArgument("session id must not be empty")
	}
	if d.Origin == "" {
		return invalidArgument("origin must not be empty")
	}
	for i := range d.Parts {
		if err := d.Parts[i].Validate(); err != nil {
			return err
		}
	}
	if d.Skill != nil && (d.Skill.SkillID == "" || d.Skill.Digest == "" || d.Skill.InvocationID == "" || d.Skill.PolicyVersion == "") {
		return invalidArgument("skill pin must name a skill, digest, invocation, and policy version")
	}
	return nil
}

func (p *Part) Validate() error {
	if p == nil {
		return invalidArgument("turn part must not be nil")
	}
	if len(p.Image) == 0 {
		return nil
	}
	if len(p.Image) > MaxImageEnvelopeBytes {
		return invalidArgument("image reference exceeds the size bound")
	}
	if !json.Valid(p.Image) {
		return invalidArgument("image reference is not valid json")
	}
	return nil
}

func invalidArgument(detail string) error {
	return &turn.Error{Code: turn.ErrorCodeInvalidArgument, Detail: detail}
}
