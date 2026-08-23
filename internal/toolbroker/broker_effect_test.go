package toolbroker

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/effect"
	"github.com/anggasct/aura/internal/store"
)

func newBrokerEffectExecutor(t *testing.T) *effect.Executor {
	t.Helper()
	db, err := store.OpenDB(context.Background(), filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now().UTC()
	if err := store.NewSessionService(db).Create(context.Background(), &store.Session{
		ID: "session-1", OwnerID: "owner-1", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	journal, err := effect.NewJournal(db, effect.Options{})
	if err != nil {
		t.Fatalf("new journal: %v", err)
	}
	executor, err := effect.NewExecutor(journal)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	return executor
}

func TestBrokerEffectfulToolUsesDurableLifecycle(t *testing.T) {
	executor := newBrokerEffectExecutor(t)
	calls := 0
	broker, err := New(&Options{
		Effects: executor,
		Adapters: map[string]Adapter{
			"exec@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
				calls++
				return ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := brokerRequest("exec", `{"command":["true"]}`, "exec-linux")
	request.EventSequence = 1
	grant, err := broker.Grant(context.Background(), request, time.Minute)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	request.Approval = &grant
	result, err := broker.Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Class != ResultOK || calls != 1 {
		t.Fatalf("result = %+v, adapter calls = %d", result, calls)
	}
	intents, err := executor.Journal().ListByState(context.Background(), effect.StateSucceeded, 0)
	if err != nil {
		t.Fatalf("list succeeded intents: %v", err)
	}
	if len(intents) != 1 || intents[0].Operation != "exec" || intents[0].Classification != effect.ClassificationEffectful {
		t.Fatalf("succeeded intents = %+v", intents)
	}
}
