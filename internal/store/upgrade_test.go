package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openDatabaseAtVersion(t *testing.T, path string, upTo int) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := OpenDB(ctx, path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, bootstrapSchemaMigrationTableSQL); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	for _, m := range migrations[:upTo] {
		if err := applyMigration(ctx, db, m); err != nil {
			t.Fatalf("apply v%d: %v", m.version, err)
		}
	}
	return db
}

func TestUpgradeBacksUpThePreviousSchemaBeforeMigrating(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := openDatabaseAtVersion(t, filepath.Join(root, "aura.db"), len(migrations)-1)
	seedMemorySession(t, db, "sess-before-upgrade", "owner-1")
	now := time.Date(2026, 11, 2, 14, 30, 0, 0, time.UTC)
	result, err := UpgradeWithBackup(ctx, db, filepath.Join(root, "backups"), root, now)
	if err != nil {
		t.Fatalf("UpgradeWithBackup: %v", err)
	}
	previous, latest := migrations[len(migrations)-2].version, migrations[len(migrations)-1].version
	if result.Applied != previous || result.Latest != latest || result.Report.Sessions != 1 {
		t.Fatalf("result = %+v", result)
	}
	if want := fmt.Sprintf("pre-upgrade-v%d-v%d-20261102T143000Z", previous, latest); result.Backup != want {
		t.Fatalf("backup = %q, want %q", result.Backup, want)
	}
	backup, err := openReadOnly(ctx, filepath.Join(root, "backups", result.Backup, backupDatabaseFilename))
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer func() { _ = backup.Close() }()
	if applied, _, err := SchemaVersions(ctx, backup); err != nil || applied != previous {
		t.Fatalf("backup schema = %d, %v; want the pre-upgrade version %d", applied, err, previous)
	}
	if applied, _, err := SchemaVersions(ctx, db); err != nil || applied != latest {
		t.Fatalf("live schema = %d, %v; want %d", applied, err, latest)
	}
}

func TestUpgradeOfACurrentSchemaWritesNoBackup(t *testing.T) {
	root := t.TempDir()
	db := openDatabaseAtVersion(t, filepath.Join(root, "aura.db"), len(migrations))
	result, err := UpgradeWithBackup(context.Background(), db, filepath.Join(root, "backups"), root, time.Now())
	if err != nil || result.Backup != "" {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(root, "backups")); !os.IsNotExist(err) {
		t.Fatalf("backup directory stat = %v, want absent", err)
	}
}

func TestUpgradeRefusesASchemaNewerThanTheBinary(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := openDatabaseAtVersion(t, filepath.Join(root, "aura.db"), len(migrations))
	if _, err := db.ExecContext(ctx, `INSERT INTO schema_migration (version, checksum, applied_at) VALUES (?, 'future', '2026-11-02T00:00:00Z')`, migrations[len(migrations)-1].version+1); err != nil {
		t.Fatalf("seed future version: %v", err)
	}
	_, err := UpgradeWithBackup(ctx, db, filepath.Join(root, "backups"), root, time.Now())
	if code, ok := CodeOf(err); !ok || code != ErrorCodeMigrationSequenceInvalid {
		t.Fatalf("err = %v, want %s", err, ErrorCodeMigrationSequenceInvalid)
	}
	if _, err := os.Stat(filepath.Join(root, "backups")); !os.IsNotExist(err) {
		t.Fatalf("backup directory stat = %v, want absent", err)
	}
}
