//go:build linux

package toolsbuiltin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/runtime/adk"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/toolbroker"
	"github.com/anggasct/aura/internal/tools"
)

func TestWiringRegistersEveryBuiltinAdapter(t *testing.T) {
	cfg := config.Default()
	cfg.Tools.Workspace = t.TempDir()
	adapters, err := builtinAdapters(cfg.Tools)
	if err != nil {
		t.Fatalf("builtinAdapters: %v", err)
	}
	want := tools.DefinitionsByKey()
	if len(adapters) != len(want) {
		t.Fatalf("registered adapters = %d, want %d", len(adapters), len(want))
	}
	for key := range want {
		if adapters[key] == nil {
			t.Errorf("builtin tool %q has no registered adapter", key)
		}
	}
}

func TestWiringConnectsEffectJournalAndEventPublisher(t *testing.T) {
	workspace := t.TempDir()
	dataRoot := t.TempDir()
	db, err := store.OpenDB(context.Background(), filepath.Join(dataRoot, "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sess := store.Session{ID: "session-1", OwnerID: "owner-1"}
	if err := store.NewSessionService(db).Create(context.Background(), &sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	cfg := config.Default()
	cfg.Tools.Workspace = workspace
	cfg.Storage.Path = dataRoot
	published := 0
	executor, err := New(&cfg, db, dataRoot, nil, nil, func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) {
		return true, nil
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if executor.journal == nil {
		t.Fatal("executor wired without an effect journal")
	}
	executor.SetEventPublisher(func(*store.RuntimeEvent) { published++ })
	if _, err := executor.Execute(context.Background(), &runtimeadk.BuiltinToolRequest{
		RequestID:       "wiring-effect-1",
		TurnID:          "turn-1",
		SessionID:       "session-1",
		PrincipalID:     "owner-1",
		ToolName:        "write_file",
		ToolVersion:     "v1",
		Arguments:       json.RawMessage(`{"path":"out.txt","content":"hello"}`),
		Capabilities:    []string{"workspace-write"},
		Trust:           string(approval.TrustOwnerInput),
		IdempotencyKey:  "wiring/effect/1",
		EventSequence:   1,
		EventInvocation: "inv-1",
		EventBranch:     "main",
		EventAuthor:     "agent",
	}); err != nil {
		t.Fatalf("execute effectful tool: %v", err)
	}
	var intents int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM effect_intent`).Scan(&intents); err != nil {
		t.Fatalf("count effect intents: %v", err)
	}
	if intents != 1 {
		t.Fatalf("effect intents = %d, want 1", intents)
	}
	if published == 0 {
		t.Fatal("event publisher was not forwarded to the effect journal")
	}
}
