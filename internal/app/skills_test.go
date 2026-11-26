package app

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/skills"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

func openSkillsTestStore(t *testing.T) *skills.SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "aura.db")
	storetest.Seed(t, dbPath)
	handle, err := sqlite.Open(t.Context(), dbPath, sqlite.Options{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = handle.Close() })
	registry, err := skills.NewSQLiteStore(handle)
	if err != nil {
		t.Fatalf("new sqlite store: %v", err)
	}
	return registry
}

func TestSkillStoreRegistryRoundTrip(t *testing.T) {
	registry := openSkillsTestStore(t)
	ctx := t.Context()
	inserted, err := registry.UpsertScan(ctx, &skills.Record{
		ID: "local/pdf@0123456789ab", Name: "pdf", Origin: `{"scope":"local"}`,
		Digest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		State:  skills.StateQuarantined, Validation: `[]`, Requested: `[]`, Granted: `[]`,
	})
	if err != nil || !inserted {
		t.Fatalf("upsert = %v, %v", inserted, err)
	}
	record, err := registry.Get(ctx, "local/pdf@0123456789ab")
	if err != nil || record.Name != "pdf" {
		t.Fatalf("get = %+v, %v", record, err)
	}
	rows, err := registry.ListByState(ctx, skills.StateQuarantined, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list = %v, %v", rows, err)
	}
	if _, err := registry.Get(ctx, "local/missing@0123456789ab"); err == nil {
		t.Errorf("missing id should fail")
	} else if code, ok := fault.CodeOf[skills.ErrorCode](err); !ok || code != skills.ErrorCodeSkillNotFound {
		t.Errorf("code = %v, %v", code, ok)
	}
}

func TestBuildSkillsEngine(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "chat.db")
	storetest.Seed(t, dbPath)
	chatDB, err := sqlite.Open(t.Context(), dbPath, sqlite.Options{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = chatDB.Close() })
	root := t.TempDir()
	skillDir := filepath.Join(root, "pdf-tools")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	document := "---\nname: pdf-tools\ndescription: Handle PDF files.\n---\nDo things.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(document), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	activations := &skillActivationLog{events: turnstore.NewEventKindReader(chatDB)}
	mod, err := skills.New(t.Context(), &skills.ModuleDeps{
		Config: &skills.Config{
			Enabled: true, Roots: []string{root}, AutoSelect: true,
			MaxIndexedSkills: 16, MaxInstructionTokens: 5000, MaxSessionContextTokens: 15000,
			MaxResourceBytes: 8388608, QuarantineRetention: configkit.Duration(1),
		},
		DB:          chatDB,
		Activations: activations,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if catalog := mod.Engine().Catalog(); len(catalog) != 0 {
		t.Errorf("quarantined skill should not index, got %v", catalog)
	}
	if _, err := skills.New(t.Context(), &skills.ModuleDeps{DB: chatDB, Activations: activations}); err == nil {
		t.Errorf("nil config should fail")
	}
}

func TestSkillStoreBusyStaysStorageBusy(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "busy.db")
	storetest.Seed(t, dbPath)
	holder, err := sqlite.Open(t.Context(), dbPath, sqlite.Options{BusyTimeout: time.Millisecond})
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	if _, err := holder.ExecContext(t.Context(), `PRAGMA locking_mode = EXCLUSIVE`); err != nil {
		t.Fatalf("exclusive mode: %v", err)
	}
	tx, err := holder.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.ExecContext(t.Context(), `DELETE FROM skill_package WHERE id = 'nonexistent'`); err != nil {
		t.Fatalf("acquire exclusive lock: %v", err)
	}
	waiter, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?_busy_timeout=1")
	if err != nil {
		t.Fatalf("open waiter: %v", err)
	}
	t.Cleanup(func() { _ = waiter.Close() })
	registry, err := skills.NewSQLiteStore(waiter)
	if err != nil {
		t.Fatalf("new sqlite store: %v", err)
	}
	_, err = registry.Get(t.Context(), "local/pdf@0123456789ab")
	if err == nil {
		t.Fatal("expected busy error, got nil")
	}
	if !fault.HasCode(err, sqlite.ErrorCodeBusy) {
		t.Fatalf("err = %v, want %s", err, sqlite.ErrorCodeBusy)
	}
	if code, ok := fault.CodeOf[skills.ErrorCode](err); ok {
		t.Fatalf("busy error was remapped to skills code %s: %v", code, err)
	}
}
