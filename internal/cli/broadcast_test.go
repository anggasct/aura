package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/store"
)

func writeBroadcastCLIConfig(t *testing.T, dataDir string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "version: 1\nstorage:\n  path: " + dataDir + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func seedBroadcastCLIItem(t *testing.T, dbPath, id, state, content string) {
	t.Helper()
	db, err := store.OpenDB(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	now := "2026-09-12T12:00:00Z"
	if _, err := db.ExecContext(t.Context(), `INSERT INTO broadcast_item (id, producer, idempotency_key, content_digest, priority, destination_alias, content_json, state, not_before, attempt_count, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, "cron", "key-"+id, "digest-"+id, "info", "default", content, state, now, 0, now, now); err != nil {
		t.Fatalf("seed item: %v", err)
	}
}

func runBroadcastCommand(t *testing.T, gf *globalFlags, args ...string) (string, error) {
	t.Helper()
	cmd := newBroadcastCmd(gf)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func TestBroadcastStatusHidesContentByDefault(t *testing.T) {
	dataDir := t.TempDir()
	seedBroadcastCLIItem(t, filepath.Join(dataDir, "aura.db"), "bcst-1", "scheduled", `{"text":"secret plans"}`)
	gf := &globalFlags{configPath: writeBroadcastCLIConfig(t, dataDir)}

	out, err := runBroadcastCommand(t, gf, "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, want := range []string{"bcst-1", "scheduled", "info", "default"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "secret plans") {
		t.Errorf("status leaks content by default:\n%s", out)
	}

	out, err = runBroadcastCommand(t, gf, "status", "--show-content")
	if err != nil {
		t.Fatalf("status --show-content: %v", err)
	}
	if !strings.Contains(out, "secret plans") {
		t.Errorf("explicit content flag hides content:\n%s", out)
	}
}

func TestBroadcastStatusFiltersState(t *testing.T) {
	dataDir := t.TempDir()
	seedBroadcastCLIItem(t, filepath.Join(dataDir, "aura.db"), "bcst-1", "scheduled", `{"text":"a"}`)
	seedBroadcastCLIItem(t, filepath.Join(dataDir, "aura.db"), "bcst-2", "held", `{"text":"b"}`)
	gf := &globalFlags{configPath: writeBroadcastCLIConfig(t, dataDir)}

	out, err := runBroadcastCommand(t, gf, "status", "--state", "held")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "bcst-2") || strings.Contains(out, "bcst-1") {
		t.Errorf("state filter wrong:\n%s", out)
	}
	if _, err := runBroadcastCommand(t, gf, "status", "--state", "flying"); err == nil {
		t.Error("invalid state accepted")
	}
}

func TestBroadcastReconcileReportsNoop(t *testing.T) {
	dataDir := t.TempDir()
	seedBroadcastCLIItem(t, filepath.Join(dataDir, "aura.db"), "bcst-1", "scheduled", `{"text":"a"}`)
	seedBroadcastCLIItem(t, filepath.Join(dataDir, "aura.db"), "bcst-9", "unknown", `{"text":"b"}`)
	gf := &globalFlags{configPath: writeBroadcastCLIConfig(t, dataDir)}

	out, err := runBroadcastCommand(t, gf, "reconcile", "bcst-9")
	if err != nil {
		t.Fatalf("reconcile unknown without effect: %v", err)
	}
	if !strings.Contains(out, "unchanged") {
		t.Errorf("reconcile output wrong:\n%s", out)
	}
	if _, err := runBroadcastCommand(t, gf, "reconcile", "missing"); err == nil {
		t.Error("missing item accepted")
	}
}
