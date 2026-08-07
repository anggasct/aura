package usage

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/store"
)

func newTestLedger(t *testing.T, dailyCap, monthlyCap int64) *Ledger {
	t.Helper()
	l, _ := newTestLedgerWithDB(t, dailyCap, monthlyCap)
	return l
}

func newTestLedgerWithDB(t *testing.T, dailyCap, monthlyCap int64) (*Ledger, *sql.DB) {
	t.Helper()
	db, err := store.OpenDB(context.Background(), t.TempDir()+"/usage.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	reg := NewPriceRegistry()
	primary := testPrice("primary")
	if err := reg.Put(primary); err != nil {
		t.Fatal(err)
	}
	auxiliary := testPrice("auxiliary")
	if err := reg.Put(auxiliary); err != nil {
		t.Fatal(err)
	}
	l, err := NewLedger(db, LedgerOptions{
		Prices:           reg,
		Currency:         "USD",
		DailyCapMicros:   dailyCap,
		MonthlyCapMicros: monthlyCap,
		ReservationTTL:   time.Hour,
		Now:              func() time.Time { return time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}
	return l, db
}

func reserve(t *testing.T, l *Ledger, invocation string, attempt int, input, maxOutput int64) *Reservation {
	t.Helper()
	r, err := l.Reserve(context.Background(), ReserveRequest{
		InvocationID:             invocation,
		Attempt:                  attempt,
		ModelDefinitionID:        "primary",
		KnownInputTokens:         input,
		RequestedMaxOutputTokens: maxOutput,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	return r
}
