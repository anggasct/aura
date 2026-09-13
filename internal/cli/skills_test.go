package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/skills"
	"github.com/anggasct/aura/internal/store"
)

func openSkillsTestDB(t *testing.T) (db store.SkillStore, cleanup func()) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "aura.db")
	handle, err := store.OpenDB(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = handle.Close() })
	if err := store.Migrate(t.Context(), handle); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return store.NewSkillStore(handle), func() { _ = handle.Close() }
}

func TestSkillStoreRegistryRoundTrip(t *testing.T) {
	inner, _ := openSkillsTestDB(t)
	adapter := &skillStoreRegistry{inner: inner}
	ctx := t.Context()
	inserted, err := adapter.UpsertScan(ctx, &skills.Record{
		ID: "local/pdf@0123456789ab", Name: "pdf", Origin: `{"scope":"local"}`,
		Digest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		State:  skills.StateQuarantined, Validation: `[]`, Requested: `[]`, Granted: `[]`,
	})
	if err != nil || !inserted {
		t.Fatalf("upsert = %v, %v", inserted, err)
	}
	record, err := adapter.Get(ctx, "local/pdf@0123456789ab")
	if err != nil || record.Name != "pdf" {
		t.Fatalf("get = %+v, %v", record, err)
	}
	rows, err := adapter.ListByState(ctx, skills.StateQuarantined, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list = %v, %v", rows, err)
	}
	if _, err := adapter.Get(ctx, "local/missing@0123456789ab"); err == nil {
		t.Errorf("missing id should fail")
	} else if code, ok := skills.CodeOf(err); !ok || code != skills.ErrorCodeSkillNotFound {
		t.Errorf("code = %v, %v", code, ok)
	}
}

func TestBuildSkillsEngine(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "chat.db")
	chatDB, err := store.OpenDB(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = chatDB.Close() })
	if err := store.Migrate(t.Context(), chatDB); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	root := t.TempDir()
	skillDir := filepath.Join(root, "pdf-tools")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	document := "---\nname: pdf-tools\ndescription: Handle PDF files.\n---\nDo things.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(document), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	engine, err := buildSkillsEngine(t.Context(), &config.Skills{
		Enabled: true, Roots: []string{root}, AutoSelect: true,
		MaxIndexedSkills: 16, MaxInstructionTokens: 5000,
		MaxResourceBytes: 8388608, QuarantineRetention: config.Duration(1),
	}, chatDB, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if catalog := engine.Catalog(); len(catalog) != 0 {
		t.Errorf("quarantined skill should not index, got %v", catalog)
	}
	if _, err := buildSkillsEngine(t.Context(), nil, chatDB, nil); err == nil {
		t.Errorf("nil config should fail")
	}
}
