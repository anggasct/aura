package app

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/memory"
	"github.com/anggasct/aura/internal/platform/clikit"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

func writeMemoryCLIConfig(t *testing.T) string {
	t.Helper()
	dataRoot := t.TempDir()
	path := dataRoot + "/config.yaml"
	content := `version: 1
storage:
  path: ` + dataRoot + `
tools:
  workspace: ` + dataRoot + `
skills:
  roots: [` + dataRoot + `]
context:
  recent_complete_turns: 5
memory:
  enabled: true
profile:
  prompt_version: v1
children:
  recovery: interrupt
`
	if err := writeFileForTest(path, content); err != nil {
		t.Fatal(err)
	}
	seedConfiguredStorage(t, path)
	return path
}

func runMemoryCommand(t *testing.T, cfg string, args ...string) (string, error) {
	t.Helper()
	restoreDefaultLogger(t)
	cmd := operationGroupCommand(&GlobalFlags{ConfigPath: cfg}, "memory")
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func seedMemoryDoc(t *testing.T, cfgPath, id, owner, session, content string, seq int64) {
	t.Helper()
	loaded, err := appconfig.Load(cfgPath)
	if err != nil {
		t.Fatalf("appconfig.Load: %v", err)
	}
	db, err := openStorage(t.Context(), loaded.Config)
	if err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	defer func() { _ = db.Close() }()
	sessions := turnstore.NewSessionService(db)
	now := time.Now().UTC()
	_ = sessions.Create(t.Context(), &turn.Session{ID: session, OwnerID: owner, CreatedAt: now, UpdatedAt: now, Metadata: []byte(`{}`)})
	memoryStore := newTestMemoryStore(t, db)
	document := &memory.StoredDocument{
		ID: id, OwnerID: owner, SessionID: session, Kind: memory.KindEventText,
		FromSequence: seq, ToSequence: seq, Content: content,
		TrustLabel: "owner_input", CreatedAt: now,
	}
	if _, err := memoryStore.UpsertDocument(t.Context(), document); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func seedMemorySummary(t *testing.T, cfgPath, id, owner, session string, expires time.Time, seq int64) {
	t.Helper()
	loaded, err := appconfig.Load(cfgPath)
	if err != nil {
		t.Fatalf("appconfig.Load: %v", err)
	}
	db, err := openStorage(t.Context(), loaded.Config)
	if err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	defer func() { _ = db.Close() }()
	sessions := turnstore.NewSessionService(db)
	now := time.Now().UTC()
	_ = sessions.Create(t.Context(), &turn.Session{ID: session, OwnerID: owner, CreatedAt: now, UpdatedAt: now, Metadata: []byte(`{}`)})
	memoryStore := newTestMemoryStore(t, db)
	document := &memory.StoredDocument{
		ID: id, OwnerID: owner, SessionID: session, Kind: memory.KindSummary,
		FromSequence: seq, ToSequence: seq, Content: "old summary content",
		TrustLabel: "owner_input", CreatedAt: now, ExpiresAt: &expires,
	}
	if _, err := memoryStore.UpsertDocument(t.Context(), document); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func TestMemoryCLIForgetSessionDryRunThenApply(t *testing.T) {
	cfg := writeMemoryCLIConfig(t)
	seedMemoryDoc(t, cfg, "mem-a1", principal.OwnerID, "sess-a", "alpha recording one", 1)
	seedMemoryDoc(t, cfg, "mem-a2", principal.OwnerID, "sess-a", "alpha recording two", 2)
	seedMemoryDoc(t, cfg, "mem-b1", principal.OwnerID, "sess-b", "beta recording one", 1)

	out, err := runMemoryCommand(t, cfg, "forget", "--session", "sess-a")
	if err != nil {
		t.Fatalf("dry-run: %v (%s)", err, out)
	}
	if !strings.Contains(out, "dry-run: 2 would be deleted") {
		t.Errorf("dry-run out = %q", out)
	}
	out, err = runMemoryCommand(t, cfg, "inspect", "--session", "sess-a")
	if err != nil || !strings.Contains(out, "mem-a1") {
		t.Errorf("dry-run wrote: %q, %v", out, err)
	}
	out, err = runMemoryCommand(t, cfg, "forget", "--session", "sess-a", "--apply")
	if err != nil || strings.TrimSpace(out) != "deleted: 2" {
		t.Fatalf("apply = %q, %v", out, err)
	}
	out, err = runMemoryCommand(t, cfg, "inspect", "--session", "sess-a")
	if err != nil || !strings.Contains(out, "no documents") {
		t.Errorf("sess-a survives: %q, %v", out, err)
	}
	out, err = runMemoryCommand(t, cfg, "inspect", "--session", "sess-b")
	if err != nil || !strings.Contains(out, "mem-b1") {
		t.Errorf("sess-b untouched: %q, %v", out, err)
	}
}

func TestMemoryCLIForgetSessionApplyMatchesPreview(t *testing.T) {
	cfg := writeMemoryCLIConfig(t)
	for seq := int64(1); seq <= 12; seq++ {
		seedMemoryDoc(t, cfg, fmt.Sprintf("mem-a%02d", seq), principal.OwnerID, "sess-a", "alpha recording", seq)
	}

	out, err := runMemoryCommand(t, cfg, "forget", "--session", "sess-a", "--limit", "10")
	if err != nil || !strings.Contains(out, "dry-run: 10 would be deleted") {
		t.Fatalf("dry-run = %q, %v", out, err)
	}
	out, err = runMemoryCommand(t, cfg, "forget", "--session", "sess-a", "--limit", "10", "--apply")
	if err != nil || strings.TrimSpace(out) != "deleted: 10" {
		t.Fatalf("apply = %q, %v", out, err)
	}
	out, err = runMemoryCommand(t, cfg, "inspect", "--session", "sess-a")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	for _, gone := range []string{"mem-a01", "mem-a05", "mem-a10"} {
		if strings.Contains(out, gone+" ") {
			t.Errorf("prepared doc %s still present: %q", gone, out)
		}
	}
	for _, kept := range []string{"mem-a11", "mem-a12"} {
		if !strings.Contains(out, kept+" ") {
			t.Errorf("unpreviewed doc %s missing: %q", kept, out)
		}
	}
}

func TestMemoryCLIForgetCrossOwnerRejected(t *testing.T) {
	cfg := writeMemoryCLIConfig(t)
	seedMemoryDoc(t, cfg, "mem-a1", "owner-foreign", "sess-a", "alpha foreign recording", 1)

	_, err := runMemoryCommand(t, cfg, "forget", "--session", "sess-a", "--apply")
	if err == nil {
		t.Fatal("cross-owner forget accepted")
	}
	out, err := runMemoryCommand(t, cfg, "inspect", "--query", "alpha")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if strings.Contains(out, "mem-a1") {
		t.Errorf("cross-owner inspect leaked: %q", out)
	}
}

func TestMemoryCLIForgetByIDAndQuery(t *testing.T) {
	cfg := writeMemoryCLIConfig(t)
	seedMemoryDoc(t, cfg, "mem-a1", principal.OwnerID, "sess-a", "alpha recording one", 1)
	seedMemoryDoc(t, cfg, "mem-a2", principal.OwnerID, "sess-a", "alpha recording two", 2)

	out, err := runMemoryCommand(t, cfg, "forget", "--id", "mem-a1")
	if err != nil || !strings.Contains(out, "dry-run: 1 would be deleted") {
		t.Fatalf("id dry-run = %q, %v", out, err)
	}
	out, err = runMemoryCommand(t, cfg, "forget", "--id", "mem-a1", "--apply")
	if err != nil || strings.TrimSpace(out) != "deleted: 1" {
		t.Fatalf("id apply = %q, %v", out, err)
	}
	out, err = runMemoryCommand(t, cfg, "forget", "--query", "alpha", "--apply")
	if err != nil || strings.TrimSpace(out) != "deleted: 1" {
		t.Fatalf("query apply = %q, %v", out, err)
	}
	out, err = runMemoryCommand(t, cfg, "inspect", "--session", "sess-a")
	if err != nil || !strings.Contains(out, "no documents") {
		t.Errorf("sess-a not empty: %q, %v", out, err)
	}
}

func TestMemoryCLIInspectProvenance(t *testing.T) {
	cfg := writeMemoryCLIConfig(t)
	seedMemoryDoc(t, cfg, "mem-a1", principal.OwnerID, "sess-a", "alpha recording one", 1)

	out, err := runMemoryCommand(t, cfg, "inspect", "--session", "sess-a")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	for _, want := range []string{"mem-a1", "session=sess-a", "kind=event_text", "range=", "trust=", "prompt=", "expires="} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect missing %q in %q", want, out)
		}
	}
	out, err = runMemoryCommand(t, cfg, "inspect", "--query", "alpha")
	if err != nil || !strings.Contains(out, "mem-a1") {
		t.Errorf("query inspect = %q, %v", out, err)
	}
}

func TestMemoryCLIPurgeBounded(t *testing.T) {
	cfg := writeMemoryCLIConfig(t)
	past := time.Now().UTC().Add(-time.Hour)
	seedMemorySummary(t, cfg, "mem-s1", principal.OwnerID, "sess-a", past, 1)
	seedMemorySummary(t, cfg, "mem-s2", principal.OwnerID, "sess-a", past, 2)

	out, err := runMemoryCommand(t, cfg, "purge", "--limit", "1", "--apply")
	if err != nil || strings.TrimSpace(out) != "purged: 1" {
		t.Fatalf("purge 1 = %q, %v", out, err)
	}
	out, err = runMemoryCommand(t, cfg, "purge", "--limit", "10", "--apply")
	if err != nil || strings.TrimSpace(out) != "purged: 1" {
		t.Fatalf("purge rest = %q, %v", out, err)
	}
}

func TestMemoryCLIPurgeListsWhatItWouldDeleteUntilApplied(t *testing.T) {
	cfg := writeMemoryCLIConfig(t)
	past := time.Now().UTC().Add(-time.Hour)
	seedMemorySummary(t, cfg, "mem-s1", principal.OwnerID, "sess-a", past, 1)

	out, err := runMemoryCommand(t, cfg, "purge")
	if err != nil || !strings.Contains(out, "dry-run: 1 would be deleted") || !strings.Contains(out, "mem-s1") {
		t.Fatalf("purge without --apply = %q, %v, want a dry-run that names the summary", out, err)
	}
	out, err = runMemoryCommand(t, cfg, "inspect", "--session", "sess-a")
	if err != nil || !strings.Contains(out, "mem-s1") {
		t.Errorf("the dry-run deleted the summary: %q, %v", out, err)
	}
	out, err = runMemoryCommand(t, cfg, "purge", "--apply")
	if err != nil || strings.TrimSpace(out) != "purged: 1" {
		t.Fatalf("purge --apply = %q, %v", out, err)
	}
	out, err = runMemoryCommand(t, cfg, "purge")
	if err != nil || !strings.Contains(out, "dry-run: 0 would be deleted") {
		t.Errorf("a purge with nothing expired = %q, %v", out, err)
	}
}

func TestMemoryCLIForgetRequiresScope(t *testing.T) {
	cfg := writeMemoryCLIConfig(t)
	_, err := runMemoryCommand(t, cfg, "forget", "--apply")
	if !clikit.IsUsageError(err) {
		t.Errorf("scope-less forget = %v, want usage error", err)
	}
	_, err = runMemoryCommand(t, cfg, "forget", "--session", "sess-a", "--id", "mem-a1")
	if !clikit.IsUsageError(err) {
		t.Errorf("dual-scope forget = %v, want usage error", err)
	}
}
