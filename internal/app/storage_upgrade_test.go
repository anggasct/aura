package app

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/store"
)

func openLiveDB(t *testing.T, dataRoot string) *sql.DB {
	t.Helper()
	db, err := store.OpenDB(context.Background(), filepath.Join(dataRoot, "aura.db"))
	if err != nil {
		t.Fatalf("open live db: %v", err)
	}
	return db
}

func execOrFail(t *testing.T, db *sql.DB, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := db.ExecContext(context.Background(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func TestStorageUpgradeInitializesAFreshInstall(t *testing.T) {
	dataRoot := t.TempDir()
	cfg := writeStorageConfig(t, dataRoot)
	if err := executeNoErr(t, "storage", "upgrade", "--config", cfg); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	db := openLiveDB(t, dataRoot)
	defer func() { _ = db.Close() }()
	if applied, latest, err := store.SchemaVersions(context.Background(), db); err != nil || applied != latest {
		t.Fatalf("schema = %d/%d, %v", applied, latest, err)
	}
	if _, err := os.Stat(filepath.Join(dataRoot, "backups")); !os.IsNotExist(err) {
		t.Fatalf("a fresh install wrote a backup: %v", err)
	}
}

func TestFailedStorageUpgradeLeavesARestorableBackup(t *testing.T) {
	dataRoot := t.TempDir()
	cfg := writeStorageConfig(t, dataRoot)
	if err := executeNoErr(t, "storage", "upgrade", "--config", cfg); err != nil {
		t.Fatalf("initial upgrade: %v", err)
	}
	db := openLiveDB(t, dataRoot)
	execOrFail(t, db,
		`CREATE TABLE upgrade_marker (note TEXT)`,
		`INSERT INTO upgrade_marker (note) VALUES ('before the upgrade')`,
		`DELETE FROM schema_migration WHERE version = (SELECT MAX(version) FROM schema_migration)`,
		`UPDATE schema_migration SET checksum = 'seeded-failure' WHERE version = (SELECT MIN(version) FROM schema_migration)`,
	)
	_ = db.Close()

	err := executeNoErr(t, "storage", "upgrade", "--config", cfg)
	if code, ok := store.CodeOf(err); !ok || code != store.ErrorCodeMigrationChecksumMismatch {
		t.Fatalf("upgrade err = %v, want %s", err, store.ErrorCodeMigrationChecksumMismatch)
	}
	if !strings.Contains(err.Error(), "restore the pre-upgrade backup pre-upgrade-v") {
		t.Fatalf("upgrade err = %v, want it to name the backup", err)
	}
	entries, readErr := os.ReadDir(filepath.Join(dataRoot, "backups"))
	if readErr != nil || len(entries) != 1 || !strings.Contains(err.Error(), entries[0].Name()) {
		t.Fatalf("backups = %v, %v", entries, readErr)
	}
	backup := filepath.Join(dataRoot, "backups", entries[0].Name())
	if err := executeNoErr(t, "storage", "verify", "--config", cfg, "--input", backup); err != nil {
		t.Fatalf("verify backup: %v", err)
	}

	db = openLiveDB(t, dataRoot)
	execOrFail(t, db, `DELETE FROM upgrade_marker`)
	_ = db.Close()
	if err := executeNoErr(t, "storage", "restore", "--config", cfg, "--input", backup, "--force"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	db = openLiveDB(t, dataRoot)
	defer func() { _ = db.Close() }()
	var note string
	if err := db.QueryRowContext(context.Background(), `SELECT note FROM upgrade_marker`).Scan(&note); err != nil || note != "before the upgrade" {
		t.Fatalf("restored marker = %q, %v", note, err)
	}
}

func TestStorageUpgradeOfACurrentSchemaIsANoOp(t *testing.T) {
	dataRoot := t.TempDir()
	cfg := writeStorageConfig(t, dataRoot)
	for range 2 {
		if err := executeNoErr(t, "storage", "upgrade", "--config", cfg); err != nil {
			t.Fatalf("upgrade: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(dataRoot, "backups")); !os.IsNotExist(err) {
		t.Fatalf("a no-op upgrade wrote a backup: %v", err)
	}
}
