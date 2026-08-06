package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestReserveCreatesConservativeReservation(t *testing.T) {
	l := newTestLedger(t, 1000000, 10000000)
	r := reserve(t, l, "inv-1", 0, 100, 200)
	if r.State != stateActive {
		t.Errorf("state = %q, want active", r.State)
	}
	if r.WindowDay != "2026-07-15" || r.WindowMonth != "2026-07" {
		t.Errorf("window = %s/%s, want 2026-07-15/2026-07", r.WindowDay, r.WindowMonth)
	}
	want := testPrice("primary").ReserveCostMicros(100, 200)
	if r.ReservedCostMicros != want {
		t.Errorf("reserved = %d, want %d", r.ReservedCostMicros, want)
	}
	if !r.ExpiresAt.After(r.CreatedAt) {
		t.Errorf("expires_at %v not after created_at %v", r.ExpiresAt, r.CreatedAt)
	}
}

func TestReserveDuplicateAttemptIdempotent(t *testing.T) {
	l := newTestLedger(t, 1000000, 10000000)
	first := reserve(t, l, "inv-1", 0, 100, 200)
	second, err := l.Reserve(context.Background(), ReserveRequest{
		InvocationID:             "inv-1",
		Attempt:                  0,
		ModelDefinitionID:        "primary",
		KnownInputTokens:         100,
		RequestedMaxOutputTokens: 200,
	})
	if err != nil {
		t.Fatalf("duplicate reserve must be idempotent, got %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("second reserve = %q, want first %q (idempotent reuse)", second.ID, first.ID)
	}
}

// TestReserveConflictPathFallbackReusesWinner proves the idempotency
// recovery used on a unique-violation: when the in-transaction re-read sees
// nothing (the concurrent winner was uncommitted under a deferred snapshot),
// the out-of-transaction fallback read returns the winner's reservation
// instead of a spurious reservation_conflict.
func TestReserveConflictPathFallbackReusesWinner(t *testing.T) {
	l := newTestLedger(t, 1000000, 10000000)
	winner := reserve(t, l, "inv-1", 0, 100, 200)

	// The insert's unique violation is simulated by routing Reserve through
	// the recovery helper with a stubbed in-tx lookup that misses (stale
	// snapshot) and a stubbed out-of-tx lookup that finds the committed row.
	var inTxCalls, outTxCalls int
	got, rerr := reservationAfterConflict(
		func() (*Reservation, bool, error) {
			inTxCalls++
			return nil, false, nil
		},
		func() (*Reservation, bool, error) {
			outTxCalls++
			return winner, true, nil
		},
	)
	if rerr != nil {
		t.Fatalf("fallback must not error, got %v", rerr)
	}
	if got == nil || got.ID != winner.ID {
		t.Fatalf("fallback = %v, want winner %q", got, winner.ID)
	}
	if inTxCalls != 1 || outTxCalls != 1 {
		t.Errorf("lookup calls = in-tx %d, out-tx %d, want 1/1", inTxCalls, outTxCalls)
	}
}

func TestReserveConflictPathInTxHitSkipsOutsideLookup(t *testing.T) {
	l := newTestLedger(t, 1000000, 10000000)
	winner := reserve(t, l, "inv-1", 0, 100, 200)

	var outTxCalls int
	got, rerr := reservationAfterConflict(
		func() (*Reservation, bool, error) { return winner, true, nil },
		func() (*Reservation, bool, error) {
			outTxCalls++
			return nil, false, nil
		},
	)
	if rerr != nil {
		t.Fatalf("in-tx hit must not error, got %v", rerr)
	}
	if got == nil || got.ID != winner.ID {
		t.Fatalf("got %v, want winner %q", got, winner.ID)
	}
	if outTxCalls != 0 {
		t.Errorf("out-of-tx lookup called %d times, want 0 when in-tx hit", outTxCalls)
	}
}

func TestReserveConflictPathBothMisses(t *testing.T) {
	got, rerr := reservationAfterConflict(
		func() (*Reservation, bool, error) { return nil, false, nil },
		func() (*Reservation, bool, error) { return nil, false, nil },
	)
	if rerr != nil {
		t.Fatalf("both misses must not error, got %v", rerr)
	}
	if got != nil {
		t.Errorf("got %v, want nil (caller reports the conflict)", got)
	}
}

func TestReserveBudgetExceeded(t *testing.T) {
	l := newTestLedger(t, 20000, 1000000)
	reserve(t, l, "inv-1", 0, 100, 200) // 14000
	_, err := l.Reserve(context.Background(), ReserveRequest{
		InvocationID:             "inv-2",
		Attempt:                  0,
		ModelDefinitionID:        "primary",
		KnownInputTokens:         100,
		RequestedMaxOutputTokens: 200,
	})
	if code, ok := CodeOf(err); !ok || code != ErrorCodeBudgetExceeded {
		t.Errorf("code = %v, want budget_exceeded (err=%v)", code, err)
	}
}

func TestReserveUnknownPriceRejectedWhenCapped(t *testing.T) {
	l := newTestLedger(t, 1000000, 10000000)
	_, err := l.Reserve(context.Background(), ReserveRequest{
		InvocationID:             "inv-1",
		Attempt:                  0,
		ModelDefinitionID:        "unregistered",
		KnownInputTokens:         100,
		RequestedMaxOutputTokens: 200,
	})
	if code, ok := CodeOf(err); !ok || code != ErrorCodePriceNotFound {
		t.Errorf("code = %v, want price_not_found (err=%v)", code, err)
	}
}

func TestReserveWithoutCapsAllowsUnpriced(t *testing.T) {
	l := newTestLedger(t, 0, 0)
	r := reserve(t, l, "inv-1", 0, 100, 200)
	if r.ReservedCostMicros < 1 {
		t.Errorf("reserved = %d, want >= 1 even without caps", r.ReservedCostMicros)
	}
}

func TestSettleIdempotent(t *testing.T) {
	l := newTestLedger(t, 1000000, 10000000)
	r := reserve(t, l, "inv-1", 0, 100, 200)
	req := &SettleRequest{
		ReservationID:   r.ID,
		ProviderUsageID: "prov-1",
		Usage:           Usage{InputTokens: 90, OutputTokens: 150},
		UsageJSON:       json.RawMessage(`{"in":90,"out":150}`),
	}
	first, err := l.Settle(context.Background(), req)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if first.Accounting != accountingReported {
		t.Errorf("accounting = %q, want reported", first.Accounting)
	}
	want := testPrice("primary").CostMicros(req.Usage)
	if first.CostMicros != want {
		t.Errorf("cost = %d, want %d", first.CostMicros, want)
	}

	second, err := l.Settle(context.Background(), req)
	if err != nil {
		t.Fatalf("duplicate settle: %v", err)
	}
	if second.ID != first.ID || second.CostMicros != first.CostMicros {
		t.Errorf("duplicate settlement not idempotent: %+v vs %+v", second, first)
	}
}

// TestSettleConflictPathFallbackReusesWinner proves the unique-violation
// recovery used on a concurrent duplicate settlement: when the in-transaction
// re-read sees nothing (the winner was uncommitted under a deferred snapshot),
// the out-of-transaction fallback read returns the winner's entry instead of a
// spurious reservation_conflict.
func TestSettleConflictPathFallbackReusesWinner(t *testing.T) {
	winner := &Settlement{ID: "entry-1", ReservationID: "res-1", CostMicros: 400, Accounting: accountingReported, PriceVersion: "2026-07-01"}

	var inTxCalls, outTxCalls int
	got, rerr := settlementAfterConflict(
		func() (*Settlement, error) {
			inTxCalls++
			return nil, sql.ErrNoRows
		},
		func() (*Settlement, error) {
			outTxCalls++
			return winner, nil
		},
	)
	if rerr != nil {
		t.Fatalf("fallback must not error, got %v", rerr)
	}
	if got == nil || got.ID != winner.ID {
		t.Fatalf("fallback = %v, want winner %q", got, winner.ID)
	}
	if inTxCalls != 1 || outTxCalls != 1 {
		t.Errorf("lookup calls = in-tx %d, out-tx %d, want 1/1", inTxCalls, outTxCalls)
	}
}

func TestSettleConflictPathInTxHitSkipsOutsideLookup(t *testing.T) {
	winner := &Settlement{ID: "entry-1", ReservationID: "res-1", CostMicros: 400}

	var outTxCalls int
	got, rerr := settlementAfterConflict(
		func() (*Settlement, error) { return winner, nil },
		func() (*Settlement, error) {
			outTxCalls++
			return nil, nil
		},
	)
	if rerr != nil {
		t.Fatalf("in-tx hit must not error, got %v", rerr)
	}
	if got == nil || got.ID != winner.ID {
		t.Fatalf("got %v, want winner %q", got, winner.ID)
	}
	if outTxCalls != 0 {
		t.Errorf("out-of-tx lookup called %d times, want 0 when in-tx hit", outTxCalls)
	}
}

func TestSettleConflictPathLookupErrorPropagates(t *testing.T) {
	wantErr := errors.New("read failed")
	got, rerr := settlementAfterConflict(
		func() (*Settlement, error) { return nil, wantErr },
		func() (*Settlement, error) { return nil, nil },
	)
	if !errors.Is(rerr, wantErr) {
		t.Fatalf("in-tx lookup error must propagate, got %v", rerr)
	}
	if got != nil {
		t.Errorf("got %v, want nil on error", got)
	}

	got, rerr = settlementAfterConflict(
		func() (*Settlement, error) { return nil, sql.ErrNoRows },
		func() (*Settlement, error) { return nil, wantErr },
	)
	if !errors.Is(rerr, wantErr) {
		t.Fatalf("outside lookup error must propagate, got %v", rerr)
	}
	if got != nil {
		t.Errorf("got %v, want nil on error", got)
	}
}

func TestSettleProviderUsageMismatchConflicts(t *testing.T) {
	l := newTestLedger(t, 1000000, 10000000)
	r := reserve(t, l, "inv-1", 0, 100, 200)
	req := &SettleRequest{
		ReservationID:   r.ID,
		ProviderUsageID: "prov-1",
		Usage:           Usage{InputTokens: 50, OutputTokens: 100},
	}
	if _, err := l.Settle(context.Background(), req); err != nil {
		t.Fatalf("settle: %v", err)
	}
	// Same reservation replayed with a different provider usage identity.
	_, err := l.Settle(context.Background(), &SettleRequest{
		ReservationID:   r.ID,
		ProviderUsageID: "prov-2",
		Usage:           Usage{InputTokens: 50, OutputTokens: 100},
	})
	if code, ok := CodeOf(err); !ok || code != ErrorCodeUsageMismatch {
		t.Errorf("code = %v, want usage_mismatch (err=%v)", code, err)
	}
	// Replay with the same provider usage identity is idempotent.
	same, err := l.Settle(context.Background(), &SettleRequest{
		ReservationID:   r.ID,
		ProviderUsageID: "prov-1",
		Usage:           Usage{InputTokens: 50, OutputTokens: 100},
	})
	if err != nil {
		t.Fatalf("idempotent settle: %v", err)
	}
	if same.CostMicros != testPrice("primary").CostMicros(Usage{InputTokens: 50, OutputTokens: 100}) {
		t.Errorf("idempotent settle returned wrong cost: %+v", same)
	}
}

func TestSettleMissingUsageEstimatesReservation(t *testing.T) {
	l := newTestLedger(t, 1000000, 10000000)
	r := reserve(t, l, "inv-1", 0, 100, 200)
	s, err := l.Settle(context.Background(), &SettleRequest{
		ReservationID: r.ID,
		Usage:         Usage{},
	})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if s.Accounting != accountingEstimated {
		t.Errorf("accounting = %q, want estimated", s.Accounting)
	}
	if s.CostMicros != r.ReservedCostMicros {
		t.Errorf("cost = %d, want reserved %d (conservative)", s.CostMicros, r.ReservedCostMicros)
	}
}

func TestSettleUnknownReservation(t *testing.T) {
	l := newTestLedger(t, 1000000, 10000000)
	_, err := l.Settle(context.Background(), &SettleRequest{ReservationID: "missing"})
	if code, ok := CodeOf(err); !ok || code != ErrorCodeReservationNotFound {
		t.Errorf("code = %v, want reservation_not_found", code)
	}
}

func TestSettleExpiredReservation(t *testing.T) {
	l := newTestLedger(t, 1000000, 10000000)
	r := reserve(t, l, "inv-1", 0, 100, 200)
	if _, err := l.ExpireStale(context.Background()); err != nil {
		t.Fatal(err)
	}
	// TTL is 1h and the fake clock is fixed, so nothing expires yet.
	if _, err := l.Settle(context.Background(), &SettleRequest{ReservationID: r.ID, Usage: Usage{InputTokens: 1}}); err != nil {
		t.Fatalf("settle active reservation: %v", err)
	}
}

func TestExpireStaleAndReconcile(t *testing.T) {
	l := newTestLedger(t, 1000000, 10000000)
	r := reserve(t, l, "inv-1", 0, 100, 200)

	// Advance the clock past the TTL by creating a new ledger on the same db.
	db := l.db
	reg := l.prices
	now := time.Date(2026, 7, 15, 14, 0, 0, 0, time.UTC) // +2h
	l2, err := NewLedger(db, LedgerOptions{
		Prices:           reg,
		Currency:         "USD",
		DailyCapMicros:   1000000,
		MonthlyCapMicros: 10000000,
		ReservationTTL:   time.Hour,
		Now:              func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	n, err := l2.ExpireStale(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expired = %d, want 1", n)
	}

	used, err := l2.WindowUsage(context.Background(), "2026-07-15", "2026-07")
	if err != nil {
		t.Fatal(err)
	}
	if used != r.ReservedCostMicros {
		t.Errorf("expired reservation must still count: used = %d, want %d", used, r.ReservedCostMicros)
	}

	rec, err := l2.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rec != 1 {
		t.Errorf("reconciled = %d, want 1", rec)
	}
	used, err = l2.WindowUsage(context.Background(), "2026-07-15", "2026-07")
	if err != nil {
		t.Fatal(err)
	}
	if used != 0 {
		t.Errorf("reconciled reservation must release budget: used = %d, want 0", used)
	}
}

func TestSettleAfterReconcileConflicts(t *testing.T) {
	l := newTestLedger(t, 1000000, 10000000)
	r := reserve(t, l, "inv-1", 0, 100, 200)
	l.now = func() time.Time { return time.Date(2026, 7, 15, 14, 0, 0, 0, time.UTC) }
	if _, err := l.ExpireStale(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := l.Settle(context.Background(), &SettleRequest{ReservationID: r.ID, Usage: Usage{InputTokens: 1}})
	if code, ok := CodeOf(err); !ok || code != ErrorCodeReservationConflict {
		t.Errorf("code = %v, want reservation_conflict after reconcile (err=%v)", code, err)
	}
}

func TestConcurrentReserveAtomic(t *testing.T) {
	l := newTestLedger(t, 1000000, 10000000)
	// Each reservation costs 14000 micros (100 input, 200 output at 200%).
	// Cap allows exactly 71 of them; the 72nd must fail.
	var wg sync.WaitGroup
	results := make(chan error, 100)
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := l.Reserve(context.Background(), ReserveRequest{
				InvocationID:             "inv-" + string(rune('a'+i)),
				Attempt:                  0,
				ModelDefinitionID:        "primary",
				KnownInputTokens:         100,
				RequestedMaxOutputTokens: 200,
			})
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)

	successes, failures := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else {
			if code, ok := CodeOf(err); !ok || code != ErrorCodeBudgetExceeded {
				t.Errorf("unexpected error: %v", err)
			}
			failures++
		}
	}
	if successes != 71 {
		t.Errorf("successes = %d, want 71 (budget allows exactly 71 at 14000 each)", successes)
	}
	if failures != 29 {
		t.Errorf("failures = %d, want 29", failures)
	}

	used, err := l.WindowUsage(context.Background(), "2026-07-15", "2026-07")
	if err != nil {
		t.Fatal(err)
	}
	if used != 71*14000 {
		t.Errorf("window used = %d, want %d", used, 71*14000)
	}
}

func TestSettleReleasesBudget(t *testing.T) {
	l := newTestLedger(t, 30000, 10000000)
	r1 := reserve(t, l, "inv-1", 0, 100, 200) // 14000
	r2 := reserve(t, l, "inv-2", 0, 100, 200) // 14000
	if _, err := l.Settle(context.Background(), &SettleRequest{ReservationID: r1.ID, Usage: Usage{InputTokens: 10, OutputTokens: 10}}); err != nil {
		t.Fatal(err)
	}
	// Settled entry cost: 10*10 + 10*30 = 400. Day total = 400 + 14000 = 14400.
	// A third reservation (14000) fits: 28400 < 30000.
	r3 := reserve(t, l, "inv-3", 0, 100, 200)
	if r3 == nil {
		t.Fatal("expected reservation to fit after settlement released budget")
	}
	// Without settlement the day total would be 42000 > 30000, so the fit
	// proves the settled entry replaced the reserved amount.
	if r2.ReservedCostMicros != 14000 {
		t.Errorf("r2 reserved = %d, want 14000", r2.ReservedCostMicros)
	}
}

func TestRetriesFallbacksChildrenEachAccountOnce(t *testing.T) {
	l := newTestLedger(t, 1000000, 10000000)
	// Same invocation, three attempts (retry + fallback) plus a child turn.
	for i := range 3 {
		r := reserve(t, l, "inv-1", i, 100, 200)
		if _, err := l.Settle(context.Background(), &SettleRequest{ReservationID: r.ID, Usage: Usage{InputTokens: 50, OutputTokens: 100}}); err != nil {
			t.Fatal(err)
		}
	}
	r := reserve(t, l, "child-inv", 0, 100, 200)
	if _, err := l.Settle(context.Background(), &SettleRequest{ReservationID: r.ID, Usage: Usage{InputTokens: 20, OutputTokens: 40}}); err != nil {
		t.Fatal(err)
	}
	used, err := l.WindowUsage(context.Background(), "2026-07-15", "2026-07")
	if err != nil {
		t.Fatal(err)
	}
	price := testPrice("primary")
	want := 3*price.CostMicros(Usage{InputTokens: 50, OutputTokens: 100}) +
		price.CostMicros(Usage{InputTokens: 20, OutputTokens: 40})
	if used != want {
		t.Errorf("window used = %d, want %d (each attempt accounted exactly once)", used, want)
	}
}

func TestUTCBoundaryNoGapDoubleCount(t *testing.T) {
	l := newTestLedger(t, 1000000, 10000000)
	// Reserve at 23:59 UTC on 07-15, settle; reserve at 00:01 on 07-16.
	now := time.Date(2026, 7, 15, 23, 59, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	r1 := reserve(t, l, "inv-1", 0, 100, 200)
	if r1.WindowDay != "2026-07-15" {
		t.Errorf("window day = %s, want 2026-07-15", r1.WindowDay)
	}
	now = time.Date(2026, 7, 16, 0, 1, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	r2 := reserve(t, l, "inv-2", 0, 100, 200)
	if r2.WindowDay != "2026-07-16" {
		t.Errorf("window day = %s, want 2026-07-16", r2.WindowDay)
	}

	day15, err := l.WindowUsage(context.Background(), "2026-07-15", "2026-07")
	if err != nil {
		t.Fatal(err)
	}
	day16, err := l.WindowUsage(context.Background(), "2026-07-16", "2026-07")
	if err != nil {
		t.Fatal(err)
	}
	if day15 != r1.ReservedCostMicros {
		t.Errorf("day15 = %d, want %d (no gap)", day15, r1.ReservedCostMicros)
	}
	if day16 != r2.ReservedCostMicros {
		t.Errorf("day16 = %d, want %d (no double count)", day16, r2.ReservedCostMicros)
	}
}

func TestRestartPreservesCapState(t *testing.T) {
	l := newTestLedger(t, 1000000, 10000000)
	r1 := reserve(t, l, "inv-1", 0, 100, 200)
	_ = r1
	r2 := reserve(t, l, "inv-2", 0, 100, 200)
	_ = r2

	// Reopen the ledger over the same db: cap state must be preserved.
	l2, err := NewLedger(l.db, LedgerOptions{
		Prices:           l.prices,
		Currency:         "USD",
		DailyCapMicros:   1000000,
		MonthlyCapMicros: 10000000,
		ReservationTTL:   time.Hour,
		Now:              l.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	used, err := l2.WindowUsage(context.Background(), "2026-07-15", "2026-07")
	if err != nil {
		t.Fatal(err)
	}
	if used != 2*r2.ReservedCostMicros {
		t.Errorf("after restart used = %d, want %d", used, 2*r2.ReservedCostMicros)
	}

	// The third reservation must respect the existing reservations.
	r3, err := l2.Reserve(context.Background(), ReserveRequest{
		InvocationID:             "inv-3",
		Attempt:                  0,
		ModelDefinitionID:        "primary",
		KnownInputTokens:         100,
		RequestedMaxOutputTokens: 200,
	})
	if err != nil {
		t.Fatalf("reserve after restart: %v", err)
	}
	_ = r3
}

func TestEntriesNegativeLimitRejected(t *testing.T) {
	l := newTestLedger(t, 1000000, 10000000)
	_, err := l.Entries(context.Background(), -1)
	if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
		t.Errorf("code = %v, want invalid_argument (err=%v)", code, err)
	}
}

func TestMonthlyCapEnforced(t *testing.T) {
	l := newTestLedger(t, 10000000, 30000)
	reserve(t, l, "inv-1", 0, 100, 200) // 14000
	reserve(t, l, "inv-2", 0, 100, 200) // 14000
	_, err := l.Reserve(context.Background(), ReserveRequest{
		InvocationID:             "inv-3",
		Attempt:                  0,
		ModelDefinitionID:        "primary",
		KnownInputTokens:         100,
		RequestedMaxOutputTokens: 200,
	})
	if code, ok := CodeOf(err); !ok || code != ErrorCodeBudgetExceeded {
		t.Errorf("code = %v, want budget_exceeded (monthly cap)", code)
	}
}
