package cli

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/channels/discord"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/telemetry"
	"github.com/anggasct/aura/internal/platform/usage"
)

const (
	budgetRateWindow  = time.Minute
	budgetRateSweepAt = 4096
	budgetLimitDaily  = "tokens_per_day"
	budgetLimitRate   = "turns_per_minute"
)

func principalTokenCaps(cfg *config.Config) map[string]int64 {
	if cfg.Access == nil {
		return nil
	}
	return map[string]int64{
		string(principal.ClassTrusted): cfg.Access.Budgets.Trusted.MaxTokensPerDay,
		string(principal.ClassPublic):  cfg.Access.Budgets.Public.MaxTokensPerDay,
	}
}

func newTurnLedger(cfg *config.Config, db *sql.DB, prices *usage.PriceRegistry, logger *slog.Logger) (*usage.Ledger, error) {
	return usage.NewLedger(db, usage.LedgerOptions{
		Prices:           prices,
		Currency:         cfg.Usage.Currency,
		DailyCapMicros:   cfg.Usage.DailyBudgetMicros,
		MonthlyCapMicros: cfg.Usage.MonthlyBudgetMicros,
		ReservationTTL:   time.Duration(cfg.Usage.ReservationTTL),
		PrincipalCaps:    principalTokenCaps(cfg),
		Logger:           logger,
	})
}

type principalTokenReader interface {
	PrincipalTokensToday(ctx context.Context, principalID string) (int64, error)
}

type principalBudgetGate struct {
	tokens   principalTokenReader
	limits   map[principal.Class]config.PrincipalBudget
	recorder *telemetry.AccessRecorder
	now      func() time.Time
	mu       sync.Mutex
	turns    map[string][]time.Time
}

func newPrincipalBudgetGate(access *config.Access, tokens principalTokenReader, recorder *telemetry.AccessRecorder) *principalBudgetGate {
	return &principalBudgetGate{
		tokens: tokens,
		limits: map[principal.Class]config.PrincipalBudget{
			principal.ClassTrusted: access.Budgets.Trusted,
			principal.ClassPublic:  access.Budgets.Public,
		},
		recorder: recorder,
		now:      time.Now,
		turns:    map[string][]time.Time{},
	}
}

func (g *principalBudgetGate) Admit(ctx context.Context, principalID string, class principal.Class) (string, error) {
	limits, ok := g.limits[class]
	if !ok {
		return "", nil
	}
	used, err := g.tokens.PrincipalTokensToday(ctx, principalID)
	if err != nil {
		return "", err
	}
	if used >= limits.MaxTokensPerDay {
		g.recorder.BudgetExhausted(ctx, string(class), budgetLimitDaily)
		return discord.BudgetRefusalExhausted, nil
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.turns) >= budgetRateSweepAt {
		for id, turns := range g.turns {
			if recent := recentTurns(turns, now); len(recent) == 0 {
				delete(g.turns, id)
			} else {
				g.turns[id] = recent
			}
		}
	}
	recent := recentTurns(g.turns[principalID], now)
	if len(recent) >= limits.MaxTurnsPerMinute {
		g.turns[principalID] = recent
		g.recorder.BudgetExhausted(ctx, string(class), budgetLimitRate)
		return discord.BudgetRefusalRateLimited, nil
	}
	g.turns[principalID] = append(recent, now)
	return "", nil
}

func recentTurns(turns []time.Time, now time.Time) []time.Time {
	cutoff := now.Add(-budgetRateWindow)
	kept := turns[:0]
	for _, at := range turns {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	return kept
}
