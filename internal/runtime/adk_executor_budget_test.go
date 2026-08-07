package runtime

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/usage"
)

// newBudgetTestLedger builds a usage ledger over the executor's test database
// with a priced "primary" model definition, so a turn run through the budget
// wrapper can reserve and settle.
func newBudgetTestLedger(t *testing.T, db *sql.DB, dailyCapMicros int64) *usage.Ledger {
	t.Helper()
	reg := usage.NewPriceRegistry()
	if err := reg.Put(&usage.Price{
		ModelDefinitionID:    "primary",
		Currency:             "USD",
		MicrosPerInputToken:  1,
		MicrosPerOutputToken: 1,
		MaxReservationRate:   100,
		EffectiveFrom:        time.Unix(0, 0).UTC(),
	}); err != nil {
		t.Fatalf("register price: %v", err)
	}
	ledger, err := usage.NewLedger(db, usage.LedgerOptions{
		Prices:           reg,
		Currency:         "USD",
		DailyCapMicros:   dailyCapMicros,
		MonthlyCapMicros: 1_000_000_000,
	})
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}
	return ledger
}

// TestADKExecutorBudgetBlocksDispatch proves an exhausted budget rejects a
// turn before the model is dispatched: the wrapper's reservation exceeds the
// cap, so the underlying model is never called.
func TestADKExecutorBudgetBlocksDispatch(t *testing.T) {
	model := &fakeADKModel{answer: "final answer", tokens: 5}
	modelName := registerFakeModel(t, model)
	db, sessions, events := newSessionTestDB(t)
	ledger := newBudgetTestLedger(t, db, 1) // no reservation fits under a 1-micro cap
	executor, err := NewADKExecutor("aura", modelName, sessions, events, &fakeBroker{}, nil, nil,
		WithBudgetLedger(ledger, "primary"))
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	mustCreateSession(t, db, "session-1")

	req := &TurnRequest{
		TurnID: "turn-1", SessionID: "session-1", PrincipalID: "user-1", Origin: OriginTerminal,
		Parts: []InputPart{{Text: "hi"}},
	}
	var sawErr error
	for _, err := range executor.Execute(context.Background(), req) {
		if err != nil {
			sawErr = err
		}
	}
	if sawErr == nil {
		t.Fatal("expected the exhausted budget to block the turn before dispatch")
	}
	if model.callCount != 0 {
		t.Errorf("model called %d times, want 0 (budget must block before dispatch)", model.callCount)
	}
}

// TestADKExecutorBudgetSettlesTurn proves a successful turn run through the
// budget wrapper creates exactly one settlement entry.
func TestADKExecutorBudgetSettlesTurn(t *testing.T) {
	model := &fakeADKModel{answer: "final answer", tokens: 7}
	modelName := registerFakeModel(t, model)
	db, sessions, events := newSessionTestDB(t)
	ledger := newBudgetTestLedger(t, db, 1_000_000_000)
	executor, err := NewADKExecutor("aura", modelName, sessions, events, &fakeBroker{}, nil, nil,
		WithBudgetLedger(ledger, "primary"))
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	mustCreateSession(t, db, "session-1")

	req := &TurnRequest{
		TurnID: "turn-1", SessionID: "session-1", PrincipalID: "user-1", Origin: OriginTerminal,
		Parts: []InputPart{{Text: "hello"}},
	}
	for _, err := range executor.Execute(context.Background(), req) {
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}

	entries, err := ledger.Entries(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1 (a successful turn settles exactly once)", len(entries))
	}
	if entries[0].ModelDefinitionID != "primary" {
		t.Errorf("entry model = %q, want primary", entries[0].ModelDefinitionID)
	}
}
