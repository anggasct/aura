package sqlite

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var sessionTableHistory = []Migration{{Version: 1, SQL: `CREATE TABLE session (id TEXT PRIMARY KEY);`}}

func openFresh(t *testing.T, opts Options) *sql.DB {
	t.Helper()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "aura.db"), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func pragmaValue(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var v string
	if err := db.QueryRowContext(context.Background(), "PRAGMA "+name).Scan(&v); err != nil {
		t.Fatalf("pragma %s: %v", name, err)
	}
	return v
}

func TestOpenDBAppliesConnectionPolicy(t *testing.T) {
	db := openFresh(t, Options{})

	if got := pragmaValue(t, db, "foreign_keys"); got != "1" {
		t.Errorf("foreign_keys = %s, want 1", got)
	}
	if got := pragmaValue(t, db, "journal_mode"); got != "wal" {
		t.Errorf("journal_mode = %s, want wal", got)
	}
	if got := pragmaValue(t, db, "busy_timeout"); got != "5000" {
		t.Errorf("busy_timeout = %s, want 5000", got)
	}
	if got := pragmaValue(t, db, "synchronous"); got != "1" {
		t.Errorf("synchronous = %s, want 1", got)
	}
}

func TestOpenDBWithOptionsHonorsBusyTimeout(t *testing.T) {
	db := openFresh(t, Options{BusyTimeout: 1500 * time.Millisecond})

	if got := pragmaValue(t, db, "busy_timeout"); got != "1500" {
		t.Errorf("busy_timeout = %s, want 1500 from options", got)
	}
	for _, p := range []struct{ name, want string }{{"foreign_keys", "1"}, {"journal_mode", "wal"}, {"synchronous", "1"}} {
		if got := pragmaValue(t, db, p.name); got != p.want {
			t.Errorf("%s = %s, want %s", p.name, got, p.want)
		}
	}
}

func TestOpenHonorsMaxOpenConnections(t *testing.T) {
	if got := openFresh(t, Options{}).Stats().MaxOpenConnections; got != 0 {
		t.Errorf("MaxOpenConnections without the option = %d, want 0 (unlimited)", got)
	}
	if got := openFresh(t, Options{MaxOpenConnections: 3}).Stats().MaxOpenConnections; got != 3 {
		t.Errorf("MaxOpenConnections = %d, want 3 from options", got)
	}
}

func TestOpenDBAppliesPolicyToEveryConnection(t *testing.T) {
	db := openFresh(t, Options{})
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(0)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var got string
			if err := db.QueryRowContext(context.Background(), "PRAGMA foreign_keys").Scan(&got); err != nil {
				t.Errorf("pragma foreign_keys: %v", err)
				return
			}
			if got != "1" {
				t.Errorf("foreign_keys = %s, want 1", got)
			}
		}()
	}
	wg.Wait()
}

func TestOpenDBPathWithSpace(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "dir with space")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dsn := filepath.Join(dir, "aura.db")
	db, err := Open(ctx, dsn, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(ctx, db, sessionTableHistory); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	ro, err := OpenReadOnly(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })
	var count int
	if err := ro.QueryRowContext(ctx, `SELECT COUNT(*) FROM session`).Scan(&count); err != nil {
		t.Fatalf("query read-only db: %v", err)
	}
}

func TestEnsureOwnerOnlySecuresExistingDatabaseSidecars(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "aura.db")
	if err := os.WriteFile(dsn, []byte("database"), 0o644); err != nil {
		t.Fatalf("write database: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(dsn+suffix, []byte("sidecar"), 0o644); err != nil {
			t.Fatalf("write %s: %v", suffix, err)
		}
	}

	if err := ensureOwnerOnly(dsn); err != nil {
		t.Fatalf("ensureOwnerOnly: %v", err)
	}
	for _, path := range []string{dsn, dsn + "-wal", dsn + "-shm"} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", filepath.Base(path), err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %o, want 600", filepath.Base(path), got)
		}
	}
}

func TestEnsureOwnerOnlyRejectsUnsafeDatabaseFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("target"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(dir, "aura.db")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink database: %v", err)
	}
	if err := ensureOwnerOnly(link); err == nil {
		t.Fatal("ensureOwnerOnly accepted a database symlink")
	}

	nonRegular := filepath.Join(dir, "directory.db")
	if err := os.Mkdir(nonRegular, 0o700); err != nil {
		t.Fatalf("mkdir database path: %v", err)
	}
	if err := ensureOwnerOnly(nonRegular); err == nil {
		t.Fatal("ensureOwnerOnly accepted a non-regular database path")
	}

	sidecarDB := filepath.Join(dir, "sidecar.db")
	if err := os.WriteFile(sidecarDB, []byte("database"), 0o600); err != nil {
		t.Fatalf("write sidecar database: %v", err)
	}
	if err := os.Symlink(target, sidecarDB+"-wal"); err != nil {
		t.Fatalf("symlink sidecar: %v", err)
	}
	if err := ensureOwnerOnly(sidecarDB); err == nil {
		t.Fatal("ensureOwnerOnly accepted a sidecar symlink")
	}
}

func TestOpenReadOnlyRejectsUnsafePermissionsWithoutMutation(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "aura.db")
	db, err := Open(context.Background(), dsn, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod directory: %v", err)
	}
	if err := os.Chmod(dsn, 0o644); err != nil {
		t.Fatalf("chmod database: %v", err)
	}

	if _, err := OpenReadOnly(context.Background(), dsn); err == nil {
		t.Fatal("OpenReadOnly accepted unsafe permissions")
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("stat directory: %v", err)
	}
	dbInfo, err := os.Lstat(dsn)
	if err != nil {
		t.Fatalf("stat database: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o755 || dbInfo.Mode().Perm() != 0o644 {
		t.Fatalf("OpenReadOnly mutated unsafe modes: directory=%o database=%o", dirInfo.Mode().Perm(), dbInfo.Mode().Perm())
	}
}

func TestOpenReadOnlyRejectsUnsafeSidecarWithoutMutation(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "aura.db")
	db, err := Open(context.Background(), dsn, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}
	sidecar := dsn + "-wal"
	if err := os.WriteFile(sidecar, []byte("sidecar"), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	if _, err := OpenReadOnly(context.Background(), dsn); err == nil {
		t.Fatal("OpenReadOnly accepted unsafe sidecar permissions")
	}
	info, err := os.Lstat(sidecar)
	if err != nil {
		t.Fatalf("stat sidecar: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("OpenReadOnly mutated sidecar mode: %o", info.Mode().Perm())
	}
}
