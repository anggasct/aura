package usage

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/platform/principal"
)

const (
	BudgetRefusalExhausted   = "budget_exhausted"
	BudgetRefusalRateLimited = "rate_limited"

	DefaultPricesFilename = "prices.yaml"

	budgetRateWindow  = time.Minute
	budgetRateSweepAt = 4096
	budgetLimitDaily  = "tokens_per_day"
	budgetLimitRate   = "turns_per_minute"
)

type BudgetExhaustedObserver func(ctx context.Context, class, limit string)

type PrincipalTokenReader interface {
	PrincipalTokensToday(ctx context.Context, principalID string) (int64, error)
}

func PrincipalTokenCaps(access *principal.Config) map[string]int64 {
	if access == nil {
		return nil
	}
	return map[string]int64{
		string(principal.ClassTrusted): access.Budgets.Trusted.MaxTokensPerDay,
		string(principal.ClassPublic):  access.Budgets.Public.MaxTokensPerDay,
	}
}

func NewTurnLedger(cfg Config, access *principal.Config, db *sql.DB, prices *PriceRegistry, logger *slog.Logger) (*Ledger, error) {
	return NewLedger(db, LedgerOptions{
		Prices:           prices,
		Currency:         cfg.Currency,
		DailyCapMicros:   cfg.DailyBudgetMicros,
		MonthlyCapMicros: cfg.MonthlyBudgetMicros,
		ReservationTTL:   time.Duration(cfg.ReservationTTL),
		PrincipalCaps:    PrincipalTokenCaps(access),
		Logger:           logger,
	})
}

func LoadPrices(ctx context.Context, logger *slog.Logger, configPath, explicit string, reg *PriceRegistry) error {
	path := explicit
	explicitGiven := explicit != ""
	if path == "" {
		if configPath == "" {
			return nil
		}
		path = filepath.Join(filepath.Dir(configPath), DefaultPricesFilename)
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) && !explicitGiven {
			return nil
		}
		return fmt.Errorf("usage: cannot read price file: %w", err)
	}
	if err := LoadPricesFile(path, reg); err != nil {
		return err
	}
	if logger != nil {
		logger.DebugContext(ctx, "loaded price records", "component", "usage", "file", filepath.Base(path), "prices", len(reg.All()))
	}
	return nil
}

type PrincipalBudgetGate struct {
	tokens   PrincipalTokenReader
	limits   map[principal.Class]principal.Budget
	observer BudgetExhaustedObserver
	now      func() time.Time
	mu       sync.Mutex
	turns    map[string][]time.Time
}

func NewPrincipalBudgetGate(access *principal.Config, tokens PrincipalTokenReader, observer BudgetExhaustedObserver) *PrincipalBudgetGate {
	return &PrincipalBudgetGate{
		tokens: tokens,
		limits: map[principal.Class]principal.Budget{
			principal.ClassTrusted: access.Budgets.Trusted,
			principal.ClassPublic:  access.Budgets.Public,
		},
		observer: observer,
		now:      time.Now,
		turns:    map[string][]time.Time{},
	}
}

func (g *PrincipalBudgetGate) WithClock(now func() time.Time) {
	if g != nil && now != nil {
		g.now = now
	}
}

func (g *PrincipalBudgetGate) Admit(ctx context.Context, principalID string, class principal.Class) (string, error) {
	limits, ok := g.limits[class]
	if !ok {
		return "", nil
	}
	used, err := g.tokens.PrincipalTokensToday(ctx, principalID)
	if err != nil {
		return "", err
	}
	if used >= limits.MaxTokensPerDay {
		if g.observer != nil {
			g.observer(ctx, string(class), budgetLimitDaily)
		}
		return BudgetRefusalExhausted, nil
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
		if g.observer != nil {
			g.observer(ctx, string(class), budgetLimitRate)
		}
		return BudgetRefusalRateLimited, nil
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
