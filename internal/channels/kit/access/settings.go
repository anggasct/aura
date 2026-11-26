package access

import (
	"context"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/approval"
)

type Settings struct {
	ApprovalTimeout  time.Duration
	StandingGrantTTL time.Duration
	Grants           approval.StandingGrantRecorder
	Pairing          PairingService
	Budgets          BudgetGate
	Erasure          PrincipalEraser
	Handoffs         HandoffGate
	Evals            EvalGate
	Denied           func(ctx context.Context, reason string)
	ApprovalRouted   func(ctx context.Context, result string)
}
