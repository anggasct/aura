package runtimeadk

import (
	"context"
	"database/sql"
	"testing"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/runtime/ingress"
	"github.com/anggasct/aura/internal/usage"
)

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

	req := &runtime.TurnRequest{
		TurnID: "turn-1", SessionID: "session-1", PrincipalID: "user-1", Origin: runtime.OriginTerminal,
		Parts: []runtimeingress.InputPart{{Text: "hi"}},
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

	req := &runtime.TurnRequest{
		TurnID: "turn-1", SessionID: "session-1", PrincipalID: "user-1", Origin: runtime.OriginTerminal,
		Parts: []runtimeingress.InputPart{{Text: "hello"}},
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

func TestADKExecutorBudgetAttributesPersona(t *testing.T) {
	model := &fakeADKModel{answer: "final answer", tokens: 7}
	modelName := registerFakeModel(t, model)
	db, sessions, events := newSessionTestDB(t)
	ledger := newBudgetTestLedger(t, db, 1_000_000_000)
	resolver := &stubResolver{definition: auraagent.Definition{ID: "kuma", Instructions: "You are Kuma."}}
	executor, err := NewADKExecutor("aura", modelName, sessions, events, &fakeBroker{}, nil, nil,
		WithBudgetLedger(ledger, "primary"), WithAgentResolver(resolver, nil))
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	mustCreateSession(t, db, "session-1")

	req := &runtime.TurnRequest{
		TurnID: "turn-1", SessionID: "session-1", PrincipalID: "user-1", Origin: runtime.OriginTerminal,
		Parts: []runtimeingress.InputPart{{Text: "hello"}}, AgentID: "kuma",
	}
	for _, err := range executor.Execute(context.Background(), req) {
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
	spends, err := ledger.StatusByAgent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(spends) != 1 || spends[0].AgentID != "kuma" || spends[0].MonthUsedMicros() == 0 {
		t.Fatalf("spend by persona = %+v, want the turn charged to kuma", spends)
	}
}

func TestADKExecutorBudgetAttributesAndCapsPrincipal(t *testing.T) {
	model := &fakeADKModel{answer: "final answer", tokens: 7}
	modelName := registerFakeModel(t, model)
	db, sessions, events := newSessionTestDB(t)
	reg := usage.NewPriceRegistry()
	ledger, err := usage.NewLedger(db, usage.LedgerOptions{Prices: reg, PrincipalCaps: map[string]int64{"trusted": 1_000_000, "public": 1}})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewADKExecutor("aura", modelName, sessions, events, &fakeBroker{}, nil, nil, WithBudgetLedger(ledger, "primary"))
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	mustCreateSession(t, db, "session-trusted")
	mustCreateSession(t, db, "session-public")
	trusted := &runtime.TurnRequest{
		TurnID: "turn-1", SessionID: "session-trusted", PrincipalID: "discord:111", PrincipalClass: principal.ClassTrusted,
		Origin: runtime.OriginTerminal, Parts: []runtimeingress.InputPart{{Text: "hello"}},
	}
	for _, err := range executor.Execute(context.Background(), trusted) {
		if err != nil {
			t.Fatalf("trusted Execute: %v", err)
		}
	}
	var principalID, class string
	if err := db.QueryRowContext(t.Context(), `SELECT principal_id, principal_class FROM usage_reservation`).Scan(&principalID, &class); err != nil {
		t.Fatal(err)
	}
	if principalID != "discord:111" || class != "trusted" {
		t.Fatalf("reservation principal = %q/%q, want discord:111/trusted", principalID, class)
	}
	calls := model.callCount
	public := &runtime.TurnRequest{
		TurnID: "turn-2", SessionID: "session-public", PrincipalID: "discord:666", PrincipalClass: principal.ClassPublic,
		Origin: runtime.OriginTerminal, Parts: []runtimeingress.InputPart{{Text: "hello"}},
	}
	var sawErr error
	for _, err := range executor.Execute(context.Background(), public) {
		if err != nil {
			sawErr = err
		}
	}
	if code, ok := usage.CodeOf(sawErr); !ok || code != usage.ErrorCodeBudgetExceeded {
		t.Fatalf("public turn err = %v, want usage_budget_exceeded", sawErr)
	}
	if model.callCount != calls {
		t.Fatalf("model calls = %d, want %d (no call for an exhausted principal)", model.callCount, calls)
	}
}
