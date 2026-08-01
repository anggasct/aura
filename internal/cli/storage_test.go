package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeStorageConfig(t *testing.T, dataRoot string) string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	content := "version: 1\nstorage:\n  path: " + dataRoot + "\n"
	if err := os.WriteFile(cfg, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfg
}

func TestStorageBackupVerifyRestoreRoundTrip(t *testing.T) {
	dataRoot := t.TempDir()
	cfg := writeStorageConfig(t, dataRoot)

	if err := executeNoErr(t, "storage", "backup", "--config", cfg, "--output", filepath.Join(dataRoot, "backup-1")); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if err := executeNoErr(t, "storage", "verify", "--config", cfg, "--input", filepath.Join(dataRoot, "backup-1")); err != nil {
		t.Fatalf("verify: %v", err)
	}

	liveDB := filepath.Join(dataRoot, "aura.db")
	if err := os.WriteFile(liveDB, []byte("will be replaced"), 0o600); err != nil {
		t.Fatalf("write live db: %v", err)
	}
	if err := executeNoErr(t, "storage", "restore", "--config", cfg, "--input", filepath.Join(dataRoot, "backup-1")); err == nil {
		t.Fatal("restore without --force over an existing db must fail")
	}
	if err := executeNoErr(t, "storage", "restore", "--config", cfg, "--input", filepath.Join(dataRoot, "backup-1"), "--force"); err != nil {
		t.Fatalf("restore --force: %v", err)
	}
}

func TestStorageReconcileAndGc(t *testing.T) {
	dataRoot := t.TempDir()
	cfg := writeStorageConfig(t, dataRoot)

	if err := executeNoErr(t, "storage", "reconcile", "--config", cfg); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := executeNoErr(t, "storage", "gc", "--config", cfg); err != nil {
		t.Fatalf("gc: %v", err)
	}
	before := time.Now().Add(-time.Hour).Format(time.RFC3339)
	if err := executeNoErr(t, "storage", "gc", "--config", cfg, "--before", before); err != nil {
		t.Fatalf("gc --before: %v", err)
	}
}

func TestStorageVerifyRequiresInput(t *testing.T) {
	dataRoot := t.TempDir()
	cfg := writeStorageConfig(t, dataRoot)
	_, err := execute(t, "storage", "verify", "--config", cfg)
	if err == nil || !strings.Contains(err.Error(), "requires --input") {
		t.Fatalf("expected usage error for verify without --input, got %v", err)
	}
}

func TestStorageGcInvalidBefore(t *testing.T) {
	dataRoot := t.TempDir()
	cfg := writeStorageConfig(t, dataRoot)
	_, err := execute(t, "storage", "gc", "--config", cfg, "--before", "not-a-time")
	if err == nil || !strings.Contains(err.Error(), "invalid --before") {
		t.Fatalf("expected invalid --before error, got %v", err)
	}
}

func TestStorageBackupRejectsInvalidConfig(t *testing.T) {
	dataRoot := t.TempDir()
	cfg := writeStorageConfig(t, dataRoot)
	if err := os.WriteFile(cfg, []byte("version: 1\nstorage:\n  max_open_connections: 99\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := executeNoErr(t, "storage", "backup", "--config", cfg); err == nil {
		t.Fatal("expected config validation error, got nil")
	}
}

func executeNoErr(t *testing.T, args ...string) error {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	return root.Execute()
}
