package usage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
)

type fakeTokenReader struct {
	used map[string]int64
	err  error
}

func (f *fakeTokenReader) PrincipalTokensToday(_ context.Context, principalID string) (int64, error) {
	return f.used[principalID], f.err
}

func testBudgetGate(tokens *fakeTokenReader, now *time.Time) *PrincipalBudgetGate {
	gate := NewPrincipalBudgetGate(&principal.Config{
		Budgets: principal.Budgets{
			Trusted: principal.Budget{MaxTokensPerDay: 1000, MaxTurnsPerMinute: 2},
			Public:  principal.Budget{MaxTokensPerDay: 100, MaxTurnsPerMinute: 1},
		},
	}, tokens, nil)
	gate.WithClock(func() time.Time { return *now })
	return gate
}

func TestPrincipalBudgetGateRateLimitSlidesOverAMinute(t *testing.T) {
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	gate := testBudgetGate(&fakeTokenReader{}, &now)
	steps := []struct {
		at   time.Duration
		want string
	}{
		{0, ""},
		{10 * time.Second, ""},
		{20 * time.Second, BudgetRefusalRateLimited},
		{59 * time.Second, BudgetRefusalRateLimited},
		{60*time.Second + time.Millisecond, ""},
		{65 * time.Second, BudgetRefusalRateLimited},
		{70*time.Second + time.Millisecond, ""},
	}
	base := now
	for _, step := range steps {
		now = base.Add(step.at)
		got, err := gate.Admit(t.Context(), "discord:1", principal.ClassTrusted)
		if err != nil || got != step.want {
			t.Fatalf("at %v Admit() = %q, %v; want %q", step.at, got, err, step.want)
		}
	}
	if got, _ := gate.Admit(t.Context(), "discord:2", principal.ClassTrusted); got != "" {
		t.Fatalf("another principal rate limited: %q", got)
	}
	if got, _ := gate.Admit(t.Context(), principal.OwnerID, principal.ClassOwner); got != "" {
		t.Fatalf("owner limited: %q", got)
	}
}

func TestPrincipalBudgetGateTokensAndFailures(t *testing.T) {
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	tokens := &fakeTokenReader{used: map[string]int64{"discord:1": 100, "discord:2": 99}}
	gate := testBudgetGate(tokens, &now)
	if got, _ := gate.Admit(t.Context(), "discord:1", principal.ClassPublic); got != BudgetRefusalExhausted {
		t.Fatalf("public at its cap = %q, want budget_exhausted", got)
	}
	if got, _ := gate.Admit(t.Context(), "discord:1", principal.ClassTrusted); got != "" {
		t.Fatalf("trusted under its own cap = %q", got)
	}
	if got, _ := gate.Admit(t.Context(), "discord:2", principal.ClassPublic); got != "" {
		t.Fatalf("public under its cap = %q", got)
	}
	if len(gate.turns["discord:1"]) != 1 {
		t.Fatalf("refused turns counted toward the rate: %v", gate.turns["discord:1"])
	}
	tokens.err = errors.New("db down")
	if _, err := gate.Admit(t.Context(), "discord:3", principal.ClassPublic); err == nil {
		t.Fatal("token lookup failure admitted")
	}
}

func TestPrincipalBudgetGateSweepsIdlePrincipals(t *testing.T) {
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	gate := testBudgetGate(&fakeTokenReader{}, &now)
	for i := range budgetRateSweepAt {
		gate.turns["discord:idle-"+string(rune(i))] = []time.Time{now.Add(-2 * time.Minute)}
	}
	if got, _ := gate.Admit(t.Context(), "discord:1", principal.ClassTrusted); got != "" {
		t.Fatalf("Admit() = %q", got)
	}
	if len(gate.turns) != 1 {
		t.Fatalf("tracked principals = %d, want idle ones swept", len(gate.turns))
	}
}

func TestPrincipalBudgetGateAndLoadPricesNilGuards(t *testing.T) {
	if gate := NewPrincipalBudgetGate(nil, &fakeTokenReader{}, nil); gate != nil {
		t.Fatalf("NewPrincipalBudgetGate(nil, ...) = %v, want nil", gate)
	}
	if gate := NewPrincipalBudgetGate(&principal.Config{}, nil, nil); gate != nil {
		t.Fatalf("NewPrincipalBudgetGate(..., nil) = %v, want nil", gate)
	}
	var nilGate *PrincipalBudgetGate
	if _, err := nilGate.Admit(t.Context(), "discord:1", principal.ClassTrusted); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("nil Admit = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	emptyGate := &PrincipalBudgetGate{}
	if _, err := emptyGate.Admit(t.Context(), "discord:1", principal.ClassTrusted); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("empty Admit = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	if err := LoadPrices(t.Context(), nil, "", "", nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("LoadPrices(nil registry) = %v, want %s", err, ErrorCodeInvalidArgument)
	}
}
