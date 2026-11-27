package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

func upgradeTestMigrations(t *testing.T) []sqlite.Migration {
	t.Helper()
	base, err := sqlite.History(turnstore.Schema(), Schema())
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	return append(base, sqlite.Migration{
		Version: len(base) + 1,
		Owner:   "storage",
		SQL:     `CREATE TABLE storage_upgrade_probe (id TEXT PRIMARY KEY);`,
	})
}

func openDatabaseAtVersion(t *testing.T, path string, migrations []sqlite.Migration, upTo int) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, path, sqlite.Options{})
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db, migrations[:upTo]); err != nil {
		t.Fatalf("migrate to v%d: %v", upTo, err)
	}
	return db
}

func TestUpgradeBacksUpThePreviousSchemaBeforeMigrating(t *testing.T) {
	migrations := upgradeTestMigrations(t)
	ctx := context.Background()
	root := t.TempDir()
	db := openDatabaseAtVersion(t, filepath.Join(root, "aura.db"), migrations, len(migrations)-1)
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
	migrations := upgradeTestMigrations(t)
	root := t.TempDir()
	db := openDatabaseAtVersion(t, filepath.Join(root, "aura.db"), migrations, len(migrations))
	result, err := UpgradeWithBackup(context.Background(), db, migrations, filepath.Join(root, "backups"), root, time.Now())
	if err != nil || result.Backup != "" {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(root, "backups")); !os.IsNotExist(err) {
		t.Fatalf("backup directory stat = %v, want absent", err)
	}
}

func TestUpgradeRefusesASchemaNewerThanTheBinary(t *testing.T) {
	migrations := upgradeTestMigrations(t)
	ctx := context.Background()
	root := t.TempDir()
	db := openDatabaseAtVersion(t, filepath.Join(root, "aura.db"), migrations, len(migrations))
	if _, err := db.ExecContext(ctx, `INSERT INTO schema_migration (version, owner, checksum, applied_at) VALUES (?, 'storage', 'future', '2026-11-02T00:00:00Z')`, migrations[len(migrations)-1].Version+1); err != nil {
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

func TestPreSquashDatabaseIsRejectedAndLeftByteIdentical(t *testing.T) {
	ctx := t.Context()
	migrations := upgradeTestMigrations(t)
	root := t.TempDir()
	dbPath := filepath.Join(root, "aura.db")

	dsn := (&url.URL{Scheme: "file", Path: dbPath}).String()
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := rawDB.ExecContext(ctx, `
PRAGMA journal_mode = DELETE;
CREATE TABLE schema_migration (
    version INTEGER PRIMARY KEY,
    checksum TEXT NOT NULL,
    applied_at TEXT NOT NULL
);
CREATE TABLE session (id TEXT PRIMARY KEY);
INSERT INTO schema_migration (version, checksum, applied_at) VALUES (1, 'legacy', '2026-08-01T00:00:00Z');
`); err != nil {
		_ = rawDB.Close()
		t.Fatalf("seed pre-squash db: %v", err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatalf("close pre-squash db: %v", err)
	}
	if err := os.Chmod(dbPath, 0o600); err != nil {
		t.Fatalf("chmod db: %v", err)
	}

	rawBytes, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read db: %v", err)
	}
	sumBefore := sha256.Sum256(rawBytes)
	hashBefore := hex.EncodeToString(sumBefore[:])
	entriesBefore, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	namesBefore := make([]string, 0, len(entriesBefore))
	for _, e := range entriesBefore {
		namesBefore = append(namesBefore, e.Name())
	}

	backupRoot := filepath.Join(root, "backups")
	if _, err := UpgradePath(ctx, dbPath, sqlite.Options{}, migrations, backupRoot, root, time.Now()); !fault.HasCode(err, sqlite.ErrorCodeSchemaIncompatible) {
		t.Fatalf("UpgradePath(pre-squash) = %v, want %s", err, sqlite.ErrorCodeSchemaIncompatible)
	}
	if _, err := os.Stat(backupRoot); !os.IsNotExist(err) {
		t.Fatalf("UpgradePath wrote backup dir: %v", err)
	}

	backupDir := filepath.Join(t.TempDir(), "pre-squash-backup")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatalf("mkdir backupDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, backupDatabaseFilename), rawBytes, 0o600); err != nil {
		t.Fatalf("write backup db: %v", err)
	}
	manifestBytes, err := json.Marshal(BackupManifest{CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, backupManifestFilename), manifestBytes, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if _, err := VerifyRestore(ctx, backupDir, root); !fault.HasCode(err, sqlite.ErrorCodeSchemaIncompatible) {
		t.Fatalf("VerifyRestore(pre-squash) = %v, want %s", err, sqlite.ErrorCodeSchemaIncompatible)
	}
	if _, err := Restore(ctx, backupDir, root, filepath.Join(t.TempDir(), "restored.db"), RestoreOptions{Force: true}); !fault.HasCode(err, sqlite.ErrorCodeSchemaIncompatible) {
		t.Fatalf("Restore(pre-squash) = %v, want %s", err, sqlite.ErrorCodeSchemaIncompatible)
	}

	afterBytes, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read db after: %v", err)
	}
	sumAfter := sha256.Sum256(afterBytes)
	if hex.EncodeToString(sumAfter[:]) != hashBefore {
		t.Fatalf("pre-squash db bytes changed")
	}
	entriesAfter, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("readdir after: %v", err)
	}
	namesAfter := make([]string, 0, len(entriesAfter))
	for _, e := range entriesAfter {
		namesAfter = append(namesAfter, e.Name())
	}
	if !slices.Equal(namesAfter, namesBefore) {
		t.Fatalf("directory entries changed: %v != %v", namesAfter, namesBefore)
	}

	inMemDB, err := sqlite.Open(ctx, ":memory:", sqlite.Options{})
	if err != nil {
		t.Fatalf("Open memory: %v", err)
	}
	t.Cleanup(func() { _ = inMemDB.Close() })
	if _, err := inMemDB.ExecContext(ctx, `
CREATE TABLE schema_migration (version INTEGER PRIMARY KEY, checksum TEXT NOT NULL, applied_at TEXT NOT NULL);
INSERT INTO schema_migration (version, checksum, applied_at) VALUES (1, 'legacy', '2026-08-01T00:00:00Z');
`); err != nil {
		t.Fatalf("seed memory pre-squash: %v", err)
	}
	if _, err := UpgradeWithBackup(ctx, inMemDB, migrations, backupRoot, root, time.Now()); !fault.HasCode(err, sqlite.ErrorCodeSchemaIncompatible) {
		t.Fatalf("UpgradeWithBackup(in-memory pre-squash) = %v, want %s", err, sqlite.ErrorCodeSchemaIncompatible)
	}
	if _, err := os.Stat(backupRoot); !os.IsNotExist(err) {
		t.Fatalf("UpgradeWithBackup wrote backup dir: %v", err)
	}
}
