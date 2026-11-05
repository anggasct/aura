package access

import (
	"context"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
)

const (
	DeniedReasonAudience    = "audience"
	DeniedReasonEvalMissing = "persona_eval_missing"
)

const (
	PairingOutcomeIssued  = "issued"
	PairingOutcomePending = "pending"
	PairingOutcomeLocked  = "locked"
)

type PairingResult struct {
	Outcome string
	Code    string
}

type PairingService interface {
	Granted(ctx context.Context, principalID, account string) (bool, error)
	Request(ctx context.Context, principalID, account string) (PairingResult, error)
}

const (
	BudgetRefusalRateLimited = config.MessageRateLimited
	BudgetRefusalExhausted   = config.MessageBudgetExhausted
)

type BudgetGate interface {
	Admit(ctx context.Context, principalID string, class principal.Class) (string, error)
}

type PrincipalEraser interface {
	Forget(ctx context.Context, principalID string) error
}

type EvalGate interface {
	Evaluated(ctx context.Context, persona string) (bool, error)
}

const (
	HandoffStateNone    = "none"
	HandoffStateOpen    = "open"
	HandoffStateExpired = "expired"
)

type HandoffGate interface {
	Check(ctx context.Context, sessionID string) (string, error)
}

const (
	DropAuthorBot          = "author_bot"
	DropEmptyContent       = "empty_content"
	DropAuthorNotAllowed   = "author_not_allowed"
	DropPairing            = "pairing"
	DropPersonaUnrouted    = "persona_unrouted"
	DropAudience           = "audience"
	DropPersonaEvalMissing = "persona_eval_missing"
	DropRateLimited        = "rate_limited"
	DropBudgetExhausted    = "budget_exhausted"
	DropBudgetCheckFailed  = "budget_check_failed"
)

func BudgetDropReason(refusal string) string {
	if refusal == BudgetRefusalRateLimited {
		return DropRateLimited
	}
	return DropBudgetExhausted
}
