package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/store/history"
)

func openDatabaseAtVersion(t *testing.T, path string, upTo int) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, path, sqlite.Options{})
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db, history.Migrations()[:upTo]); err != nil {
		t.Fatalf("migrate to v%d: %v", upTo, err)
	}
	return db
}

func TestUpgradeBacksUpThePreviousSchemaBeforeMigrating(t *testing.T) {
	migrations := history.Migrations()
	ctx := context.Background()
	root := t.TempDir()
	db := openDatabaseAtVersion(t, filepath.Join(root, "aura.db"), len(migrations)-1)
	mustCreateSession(t, db, "sess-before-upgrade")
	now := time.Date(2026, 11, 2, 14, 30, 0, 0, time.UTC)
	result, err := UpgradeWithBackup(ctx, db, migrations, filepath.Join(root, "backups"), root, now)
	if err != nil {
		t.Fatalf("UpgradeWithBackup: %v", err)
	}
	previous, latest := migrations[len(migrations)-2].Version, migrations[len(migrations)-1].Version
	if result.Applied != previous || result.Latest != latest || result.Report.Sessions != 1 {
		t.Fatalf("result = %+v", result)
	}
	if want := fmt.Sprintf("pre-upgrade-v%d-v%d-20261102T143000Z", previous, latest); result.Backup != want {
		t.Fatalf("backup = %q, want %q", result.Backup, want)
	}
	backup, err := sqlite.OpenReadOnly(ctx, filepath.Join(root, "backups", result.Backup, backupDatabaseFilename))
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer func() { _ = backup.Close() }()
	if applied, _, err := sqlite.SchemaVersions(ctx, backup, migrations); err != nil || applied != previous {
		t.Fatalf("backup schema = %d, %v; want the pre-upgrade version %d", applied, err, previous)
	}
	if applied, _, err := sqlite.SchemaVersions(ctx, db, migrations); err != nil || applied != latest {
		t.Fatalf("live schema = %d, %v; want %d", applied, err, latest)
	}
}

func TestUpgradeOfACurrentSchemaWritesNoBackup(t *testing.T) {
	migrations := history.Migrations()
	root := t.TempDir()
	db := openDatabaseAtVersion(t, filepath.Join(root, "aura.db"), len(migrations))
	result, err := UpgradeWithBackup(context.Background(), db, migrations, filepath.Join(root, "backups"), root, time.Now())
	if err != nil || result.Backup != "" {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(root, "backups")); !os.IsNotExist(err) {
		t.Fatalf("backup directory stat = %v, want absent", err)
	}
}

func TestUpgradeRefusesASchemaNewerThanTheBinary(t *testing.T) {
	migrations := history.Migrations()
	ctx := context.Background()
	root := t.TempDir()
	db := openDatabaseAtVersion(t, filepath.Join(root, "aura.db"), len(migrations))
	if _, err := db.ExecContext(ctx, `INSERT INTO schema_migration (version, checksum, applied_at) VALUES (?, 'future', '2026-11-02T00:00:00Z')`, migrations[len(migrations)-1].Version+1); err != nil {
		t.Fatalf("seed future version: %v", err)
	}
	_, err := UpgradeWithBackup(ctx, db, migrations, filepath.Join(root, "backups"), root, time.Now())
	if !fault.HasCode(err, sqlite.ErrorCodeMigrationSequenceInvalid) {
		t.Fatalf("err = %v, want %s", err, sqlite.ErrorCodeMigrationSequenceInvalid)
	}
	if _, err := os.Stat(filepath.Join(root, "backups")); !os.IsNotExist(err) {
		t.Fatalf("backup directory stat = %v, want absent", err)
	}
}
