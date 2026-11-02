package usage

import (
	"context"
	"sync"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func newPrincipalLedger(t *testing.T, now *time.Time) *Ledger {
	t.Helper()
	base, _ := newTestLedgerWithDB(t, 0, 0)
	l, err := NewLedger(base.db, LedgerOptions{
		Prices:         base.prices,
		Currency:       "USD",
		ReservationTTL: time.Hour,
		PrincipalCaps:  map[string]int64{"trusted": 1000, "public": 300},
		Now:            func() time.Time { return *now },
	})
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}
	return l
}

func principalReserve(l *Ledger, invocation, principalID, class string, input, maxOutput int64) (*Reservation, error) {
	return l.Reserve(context.Background(), &ReserveRequest{
		InvocationID:             invocation,
		ModelDefinitionID:        "primary",
		AgentID:                  "kantor",
		PrincipalID:              principalID,
		PrincipalClass:           class,
		KnownInputTokens:         input,
		RequestedMaxOutputTokens: maxOutput,
	})
}

func TestReservePrincipalDailyTokenCap(t *testing.T) {
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	l := newPrincipalLedger(t, &now)
	if _, err := principalReserve(l, "inv-1", "discord:1", "public", 100, 100); err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	if _, err := principalReserve(l, "inv-2", "discord:1", "public", 50, 100); err == nil {
		t.Fatal("reserve over the public cap accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeBudgetExceeded {
		t.Fatalf("over-cap code = %v, %v; want usage_budget_exceeded", code, ok)
	}
	if _, err := principalReserve(l, "inv-3", "discord:1", "public", 50, 50); err != nil {
		t.Fatalf("reserve exactly at the cap: %v", err)
	}
	if _, err := principalReserve(l, "inv-4", "discord:2", "public", 100, 100); err != nil {
		t.Fatalf("another principal is capped by discord:1 usage: %v", err)
	}
	if _, err := principalReserve(l, "inv-5", "discord:3", "trusted", 400, 500); err != nil {
		t.Fatalf("trusted reserve under its own cap: %v", err)
	}
	for i, class := range []string{"owner", ""} {
		if _, err := principalReserve(l, "inv-owner-"+string(rune('a'+i)), "local-owner", class, 5000, 5000); err != nil {
			t.Fatalf("class %q reserve capped: %v", class, err)
		}
	}
	used, err := l.PrincipalTokensToday(context.Background(), "discord:1")
	if err != nil || used != 300 {
		t.Fatalf("PrincipalTokensToday() = %d, %v; want 300", used, err)
	}
	var principalID, class string
	var tokens int64
	if err := l.db.QueryRowContext(t.Context(), `SELECT principal_id, principal_class, reserved_tokens FROM usage_reservation WHERE invocation_id = 'inv-1'`).Scan(&principalID, &class, &tokens); err != nil {
		t.Fatal(err)
	}
	if principalID != "discord:1" || class != "public" || tokens != 200 {
		t.Fatalf("reservation attribution = %q/%q/%d", principalID, class, tokens)
	}
}

func TestPrincipalTokensCountSettledUsageAndExpiredHolds(t *testing.T) {
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	l := newPrincipalLedger(t, &now)
	settled, err := principalReserve(l, "inv-1", "discord:1", "public", 100, 200)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Settle(context.Background(), &SettleRequest{ReservationID: settled.ID, Usage: Usage{InputTokens: 40, OutputTokens: 20}}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if _, err := principalReserve(l, "inv-2", "discord:1", "public", 30, 30); err != nil {
		t.Fatal(err)
	}
	if used, _ := l.PrincipalTokensToday(context.Background(), "discord:1"); used != 120 {
		t.Fatalf("used = %d, want settled 60 + active 60", used)
	}
	now = now.Add(2 * time.Hour)
	if _, err := l.ExpireStale(context.Background()); err != nil {
		t.Fatal(err)
	}
	if used, _ := l.PrincipalTokensToday(context.Background(), "discord:1"); used != 120 {
		t.Fatalf("used after expiry = %d, want the expired hold still counted", used)
	}
	if _, err := l.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if used, _ := l.PrincipalTokensToday(context.Background(), "discord:1"); used != 60 {
		t.Fatalf("used after reconcile = %d, want settled usage only", used)
	}
	now = time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	if used, _ := l.PrincipalTokensToday(context.Background(), "discord:1"); used != 0 {
		t.Fatalf("used on the next UTC day = %d, want 0", used)
	}
	if _, err := principalReserve(l, "inv-3", "discord:1", "public", 100, 200); err != nil {
		t.Fatalf("reserve after rollover: %v", err)
	}
}

func TestConcurrentPrincipalReserveAtomic(t *testing.T) {
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	l := newPrincipalLedger(t, &now)
	var wg sync.WaitGroup
	results := make(chan error, 40)
	for i := range 40 {
		wg.Go(func() {
			_, err := principalReserve(l, "inv-"+string(rune('A'+i)), "discord:1", "trusted", 50, 50)
			results <- err
		})
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if code, ok := CodeOf(err); !ok || code != ErrorCodeBudgetExceeded {
			t.Errorf("unexpected error: %v", err)
		}
	}
	if successes != 10 {
		t.Fatalf("successes = %d, want 10 (1000-token cap at 100 each)", successes)
	}
	if used, _ := l.PrincipalTokensToday(context.Background(), "discord:1"); used != 1000 {
		t.Fatalf("used = %d, want 1000", used)
	}
}

func TestBudgetedStopsCallingTheModelOnceThePrincipalIsExhausted(t *testing.T) {
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	l := newPrincipalLedger(t, &now)
	inner := &fakeLLM{name: "fake", usage: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 50, CandidatesTokenCount: 100}}
	b, err := NewBudgeted(inner, l, "primary", Attribution{AgentID: "kantor", PrincipalID: "discord:1", PrincipalClass: "public"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hi"}}}},
		Config:   &genai.GenerateContentConfig{MaxOutputTokens: 100},
	}
	var lastErr error
	for range 3 {
		lastErr = nil
		for _, err := range b.GenerateContent(context.Background(), req, false) {
			if err != nil {
				lastErr = err
			}
		}
	}
	if code, ok := CodeOf(lastErr); !ok || code != ErrorCodeBudgetExceeded {
		t.Fatalf("third call err = %v, want usage_budget_exceeded", lastErr)
	}
	if calls := inner.callCount(); calls != 2 {
		t.Fatalf("model calls = %d, want 2 and none after exhaustion", calls)
	}
}
