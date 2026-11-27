package sqlite

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
)

func sha256File(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

func createPreSquashRollbackDB(t *testing.T, path string) {
	t.Helper()
	dsn := (&url.URL{Scheme: "file", Path: path}).String()
	db, err := sql.Open(sqlDriverName, dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := t.Context()
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode = DELETE`); err != nil {
		t.Fatalf("set journal_mode: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE schema_migration (
    version INTEGER PRIMARY KEY,
    checksum TEXT NOT NULL,
    applied_at TEXT NOT NULL
);
CREATE TABLE session (id TEXT PRIMARY KEY);
INSERT INTO schema_migration (version, checksum, applied_at) VALUES (1, 'abc', '2026-08-01T00:00:00Z');
`); err != nil {
		t.Fatalf("seed pre-squash db: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close pre-squash db: %v", err)
	}
}

func createPreSquashWALDBWithoutSHM(t *testing.T, path string) {
	t.Helper()
	dsn := (&url.URL{Scheme: "file", Path: path}).String()
	db, err := sql.Open(sqlDriverName, dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	ctx := t.Context()
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode = WAL`); err != nil {
		_ = db.Close()
		t.Fatalf("set journal_mode: %v", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA wal_autocheckpoint = 0`); err != nil {
		_ = db.Close()
		t.Fatalf("disable autocheckpoint: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE schema_migration (
    version INTEGER PRIMARY KEY,
    checksum TEXT NOT NULL,
    applied_at TEXT NOT NULL
);
CREATE TABLE session (id TEXT PRIMARY KEY);
INSERT INTO schema_migration (version, checksum, applied_at) VALUES (1, 'abc', '2026-08-01T00:00:00Z');
`); err != nil {
		_ = db.Close()
		t.Fatalf("seed pre-squash wal db: %v", err)
	}
	srcDB, err := os.ReadFile(path)
	if err != nil {
		_ = db.Close()
		t.Fatalf("read wal db: %v", err)
	}
	srcWAL, err := os.ReadFile(path + "-wal")
	if err != nil {
		_ = db.Close()
		t.Fatalf("read wal sidecar: %v", err)
	}
	if len(srcWAL) == 0 {
		_ = db.Close()
		t.Fatal("expected non-empty -wal file")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close wal db: %v", err)
	}
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
	if err := os.WriteFile(path, srcDB, 0o600); err != nil {
		t.Fatalf("write frozen db: %v", err)
	}
	if err := os.WriteFile(path+"-wal", srcWAL, 0o600); err != nil {
		t.Fatalf("write frozen wal: %v", err)
	}
}

func TestCheckCompatibilityBeforeAndAfterOpen(t *testing.T) {
	ctx := t.Context()

	dirBefore := t.TempDir()
	dbBefore := filepath.Join(dirBefore, "pre-before.db")
	createPreSquashRollbackDB(t, dbBefore)
	hashBefore := sha256File(t, dbBefore)
	entriesBefore := dirEntries(t, dirBefore)

	if err := CheckCompatibility(ctx, dbBefore); !fault.HasCode(err, ErrorCodeSchemaIncompatible) {
		t.Fatalf("CheckCompatibility before Open = %v, want %s", err, ErrorCodeSchemaIncompatible)
	}
	if got := sha256File(t, dbBefore); got != hashBefore {
		t.Fatalf("db bytes mutated before Open: %s != %s", got, hashBefore)
	}
	if got := dirEntries(t, dirBefore); !slices.Equal(got, entriesBefore) {
		t.Fatalf("dir entries mutated before Open: %v != %v", got, entriesBefore)
	}

	unrelated, err := Open(ctx, filepath.Join(t.TempDir(), "unrelated.db"), Options{})
	if err != nil {
		t.Fatalf("Open unrelated: %v", err)
	}
	t.Cleanup(func() { _ = unrelated.Close() })

	if err := CheckCompatibility(ctx, dbBefore); !fault.HasCode(err, ErrorCodeSchemaIncompatible) {
		t.Fatalf("CheckCompatibility after Open = %v, want %s", err, ErrorCodeSchemaIncompatible)
	}
	if got := sha256File(t, dbBefore); got != hashBefore {
		t.Fatalf("db bytes mutated after Open: %s != %s", got, hashBefore)
	}
	if got := dirEntries(t, dirBefore); !slices.Equal(got, entriesBefore) {
		t.Fatalf("dir entries mutated after Open: %v != %v", got, entriesBefore)
	}
}

func TestCheckCompatibilityWALWithoutSHMLeavesZeroLiveMutation(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "wal-pre.db")
	createPreSquashWALDBWithoutSHM(t, dbPath)

	dbHash := sha256File(t, dbPath)
	walHash := sha256File(t, dbPath+"-wal")
	entries := dirEntries(t, dir)
	if slices.Contains(entries, "wal-pre.db-shm") {
		t.Fatalf("fixture must not have -shm: %v", entries)
	}

	if err := CheckCompatibility(ctx, dbPath); !fault.HasCode(err, ErrorCodeSchemaIncompatible) {
		t.Fatalf("CheckCompatibility(WAL) = %v, want %s", err, ErrorCodeSchemaIncompatible)
	}
	if _, err := OpenAndMigrate(ctx, dbPath, Options{}, sessionTableHistory); !fault.HasCode(err, ErrorCodeSchemaIncompatible) {
		t.Fatalf("OpenAndMigrate(WAL) = %v, want %s", err, ErrorCodeSchemaIncompatible)
	}
	if got := sha256File(t, dbPath); got != dbHash {
		t.Fatalf("db mutated: %s != %s", got, dbHash)
	}
	if got := sha256File(t, dbPath+"-wal"); got != walHash {
		t.Fatalf("wal mutated: %s != %s", got, walHash)
	}
	if got := dirEntries(t, dir); !slices.Equal(got, entries) {
		t.Fatalf("dir entries changed (created -shm?): %v != %v", got, entries)
	}
}

func TestCheckCompatibilityZeroByteSymlinkRejected(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	target := filepath.Join(dir, "empty-target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatalf("write empty target: %v", err)
	}
	link := filepath.Join(dir, "aura.db")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := CheckCompatibility(ctx, link); !fault.HasCode(err, ErrorCodeUnavailable) {
		t.Fatalf("CheckCompatibility(0-byte symlink) = %v, want %s", err, ErrorCodeUnavailable)
	}
}

func TestCheckCompatibilityPermissionsPreservedOnIncompatibleAndTightenedOnCompatible(t *testing.T) {
	ctx := t.Context()

	incompatDir := filepath.Join(t.TempDir(), "incompat")
	if err := os.MkdirAll(incompatDir, 0o755); err != nil {
		t.Fatalf("mkdir incompat: %v", err)
	}
	incompatDB := filepath.Join(incompatDir, "aura.db")
	createPreSquashRollbackDB(t, incompatDB)
	if err := os.Chmod(incompatDir, 0o755); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	if err := os.Chmod(incompatDB, 0o644); err != nil {
		t.Fatalf("chmod db: %v", err)
	}
	hashBefore := sha256File(t, incompatDB)

	if _, err := OpenAndMigrate(ctx, incompatDB, Options{}, sessionTableHistory); !fault.HasCode(err, ErrorCodeSchemaIncompatible) {
		t.Fatalf("OpenAndMigrate(incompat 0644) = %v, want %s", err, ErrorCodeSchemaIncompatible)
	}
	if got := sha256File(t, incompatDB); got != hashBefore {
		t.Fatalf("incompat db bytes mutated")
	}
	if info, err := os.Lstat(incompatDir); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("incompat dir mode = %o, %v; want 755", info.Mode().Perm(), err)
	}
	if info, err := os.Lstat(incompatDB); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("incompat db mode = %o, %v; want 644", info.Mode().Perm(), err)
	}

	compatDir := filepath.Join(t.TempDir(), "compat")
	if err := os.MkdirAll(compatDir, 0o755); err != nil {
		t.Fatalf("mkdir compat: %v", err)
	}
	compatDB := filepath.Join(compatDir, "aura.db")
	db, err := OpenAndMigrate(ctx, compatDB, Options{}, sessionTableHistory)
	if err != nil {
		t.Fatalf("initial OpenAndMigrate: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close compat db: %v", err)
	}
	if err := os.Chmod(compatDir, 0o755); err != nil {
		t.Fatalf("chmod compat dir: %v", err)
	}
	if err := os.Chmod(compatDB, 0o644); err != nil {
		t.Fatalf("chmod compat db: %v", err)
	}
	reopened, err := OpenAndMigrate(ctx, compatDB, Options{}, sessionTableHistory)
	if err != nil {
		t.Fatalf("reopen OpenAndMigrate(compat 0644): %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	if info, err := os.Lstat(compatDir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("compat dir mode = %o, %v; want 700", info.Mode().Perm(), err)
	}
	if info, err := os.Lstat(compatDB); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("compat db mode = %o, %v; want 600", info.Mode().Perm(), err)
	}
}

func TestCheckCompatibilityNonSQLiteReturnsUnavailable(t *testing.T) {
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "garbage.db")
	if err := os.WriteFile(dbPath, []byte("this is definitely not a sqlite database file"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	err := CheckCompatibility(ctx, dbPath)
	if !fault.HasCode(err, ErrorCodeUnavailable) {
		t.Fatalf("CheckCompatibility(garbage) = %v, want %s", err, ErrorCodeUnavailable)
	}
	if fault.HasCode(err, ErrorCodeSchemaIncompatible) {
		t.Fatalf("non-SQLite file must not be classified as %s", ErrorCodeSchemaIncompatible)
	}
}

func TestCheckCompatibilityUnversionedDatabaseRejected(t *testing.T) {
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "unversioned.db")
	dsn := (&url.URL{Scheme: "file", Path: dbPath}).String()
	db, err := sql.Open(sqlDriverName, dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE stray (id TEXT PRIMARY KEY)`); err != nil {
		_ = db.Close()
		t.Fatalf("create stray table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := CheckCompatibility(ctx, dbPath); !fault.HasCode(err, ErrorCodeSchemaIncompatible) {
		t.Fatalf("CheckCompatibility(unversioned) = %v, want %s", err, ErrorCodeSchemaIncompatible)
	}
}

func TestCheckCompatibilityAcceptsMissingEmptyAndMemory(t *testing.T) {
	ctx := t.Context()
	if err := CheckCompatibility(ctx, ""); err != nil {
		t.Fatalf("CheckCompatibility empty path = %v", err)
	}
	if err := CheckCompatibility(ctx, ":memory:"); err != nil {
		t.Fatalf("CheckCompatibility :memory: = %v", err)
	}
	if err := CheckCompatibility(ctx, filepath.Join(t.TempDir(), "missing-parent", "aura.db")); err != nil {
		t.Fatalf("CheckCompatibility missing parent = %v", err)
	}
	emptyFile := filepath.Join(t.TempDir(), "empty.db")
	if err := os.WriteFile(emptyFile, nil, 0o600); err != nil {
		t.Fatalf("write empty file: %v", err)
	}
	if err := CheckCompatibility(ctx, emptyFile); err != nil {
		t.Fatalf("CheckCompatibility empty file = %v", err)
	}
}

func TestMigrateAndSchemaVersionsRejectInMemoryPreSquashDB(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, ":memory:", Options{})
	if err != nil {
		t.Fatalf("Open memory: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, `
CREATE TABLE schema_migration (
    version INTEGER PRIMARY KEY,
    checksum TEXT NOT NULL,
    applied_at TEXT NOT NULL
);
INSERT INTO schema_migration (version, checksum, applied_at) VALUES (1, 'abc', '2026-08-01T00:00:00Z');
`); err != nil {
		t.Fatalf("seed pre-squash memory db: %v", err)
	}
	if err := Migrate(ctx, db, sessionTableHistory); !fault.HasCode(err, ErrorCodeSchemaIncompatible) {
		t.Fatalf("Migrate(in-memory pre-squash) = %v, want %s", err, ErrorCodeSchemaIncompatible)
	}
	if _, _, err := SchemaVersions(ctx, db, sessionTableHistory); !fault.HasCode(err, ErrorCodeSchemaIncompatible) {
		t.Fatalf("SchemaVersions(in-memory pre-squash) = %v, want %s", err, ErrorCodeSchemaIncompatible)
	}
}
