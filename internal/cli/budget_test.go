package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/channel/discord"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/usage"
)

type fakeTokenReader struct {
	used map[string]int64
	err  error
}

func (f *fakeTokenReader) PrincipalTokensToday(_ context.Context, principalID string) (int64, error) {
	return f.used[principalID], f.err
}

func testBudgetGate(tokens *fakeTokenReader, now *time.Time) *principalBudgetGate {
	return &principalBudgetGate{
		tokens: tokens,
		limits: map[principal.Class]config.PrincipalBudget{
			principal.ClassTrusted: {MaxTokensPerDay: 1000, MaxTurnsPerMinute: 2},
			principal.ClassPublic:  {MaxTokensPerDay: 100, MaxTurnsPerMinute: 1},
		},
		now:   func() time.Time { return *now },
		turns: map[string][]time.Time{},
	}
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
		{20 * time.Second, discord.BudgetRefusalRateLimited},
		{59 * time.Second, discord.BudgetRefusalRateLimited},
		{60*time.Second + time.Millisecond, ""},
		{65 * time.Second, discord.BudgetRefusalRateLimited},
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
	if got, _ := gate.Admit(t.Context(), "discord:1", principal.ClassPublic); got != discord.BudgetRefusalExhausted {
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

func TestPrincipalBudgetGateReadsTheTurnLedger(t *testing.T) {
	cfg := writeChildrenCLIConfig(t)
	db := openAccessDB(t, cfg)
	loaded, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Config.Access.Budgets.Public.MaxTokensPerDay = 150
	prices := usage.NewPriceRegistry()
	if err := prices.Put(&usage.Price{
		ModelDefinitionID: "primary", Currency: loaded.Config.Usage.Currency, MicrosPerInputToken: 1, MicrosPerOutputToken: 1,
		MaxReservationRate: 100, EffectiveFrom: time.Unix(0, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	ledger, err := newTurnLedger(loaded.Config, db, prices, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Reserve(t.Context(), &usage.ReserveRequest{
		InvocationID: "inv-1", ModelDefinitionID: "primary", PrincipalID: "discord:9", PrincipalClass: "public",
		KnownInputTokens: 50, RequestedMaxOutputTokens: 100,
	}); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if _, err := ledger.Reserve(t.Context(), &usage.ReserveRequest{
		InvocationID: "inv-2", ModelDefinitionID: "primary", PrincipalID: "discord:9", PrincipalClass: "public",
		KnownInputTokens: 1, RequestedMaxOutputTokens: 1,
	}); err == nil {
		t.Fatal("turn ledger ignored the configured public cap")
	}
	settings := discordAccessSettings(loaded.Config, db, nil)
	if settings.Budgets == nil {
		t.Fatal("discord access settings carry no budget gate")
	}
	if got, err := settings.Budgets.Admit(t.Context(), "discord:9", principal.ClassPublic); err != nil || got != discord.BudgetRefusalExhausted {
		t.Fatalf("Admit() = %q, %v; want budget_exhausted from the shared ledger", got, err)
	}
}
