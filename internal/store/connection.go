package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite"
)

const (
	sqlDriverName         = "sqlite"
	defaultBusyTimeoutMS  = 5000
	busyTimeoutQueryParam = "_busy_timeout"
	immediateTxlockParam  = "_txlock"
)

var registerConnectionPolicyOnce sync.Once

var staticPragmaSet = []struct {
	name string
	set  string
	want string
}{
	// foreign_keys, journal_mode, and synchronous are fixed by contract;
	// busy_timeout is read from the DSN so config can tune it per store.
	{name: "foreign_keys", set: "PRAGMA foreign_keys = ON", want: "1"},
	{name: "journal_mode", set: "PRAGMA journal_mode = WAL", want: "wal"},
	{name: "synchronous", set: "PRAGMA synchronous = NORMAL", want: "1"},
}

type OpenOptions struct {
	// BusyTimeout is the per-connection busy timeout; zero selects the
	// contract default of 5s.
	BusyTimeout time.Duration
}

// OpenDB opens a SQLite database with the contract connection policy and
// installs a connection hook so every physical connection the pool creates -
// not just the first - gets foreign keys, WAL, busy timeout, and synchronous
// mode applied and verified before it is handed to a caller. Write
// transactions begin immediately, so a transaction that reads before writing
// waits for the write lock instead of failing on a stale snapshot.
func OpenDB(ctx context.Context, dsn string) (*sql.DB, error) {
	return OpenDBWithOptions(ctx, dsn, OpenOptions{})
}

// OpenDBWithOptions is OpenDB with per-store tuning (busy timeout) encoded in
// the DSN, so the connection hook applies the configured value instead of the
// default.
func OpenDBWithOptions(ctx context.Context, dsn string, opts OpenOptions) (*sql.DB, error) {
	registerConnectionPolicyOnce.Do(func() {
		sqlite.RegisterConnectionHook(verifyConnectionPolicy)
	})
	if dsn != "" && dsn != ":memory:" {
		if err := ensureOwnerOnly(dsn); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open(sqlDriverName, withConnectionOptions(dsn, opts))
	if err != nil {
		return nil, codedError(ErrorCodeStorageUnavailable, "open sqlite database", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, codedError(ErrorCodeStorageUnavailable, "open sqlite database", err)
	}
	return db, nil
}

// ensureOwnerOnly makes the storage directory and database file owner-only
// before SQLite opens them, so the WAL and SHM siblings are created with the
// database's own mode instead of the process default.
func ensureOwnerOnly(path string) error {
	dir := filepath.Dir(path)
	if err := ensureOwnerOnlyDirectory(dir); err != nil {
		return err
	}
	if err := ensureOwnerOnlyFile(path, true, "database"); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := ensureOwnerOnlyFile(path+suffix, false, "database sidecar"); err != nil {
			return err
		}
	}
	return nil
}

func ensureOwnerOnlyDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return codedError(ErrorCodeStorageUnavailable, "create storage directory", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return codedError(ErrorCodeStorageUnavailable, "inspect storage directory", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return codedError(ErrorCodeStorageUnavailable, "storage path is not a directory", errors.New("unsafe storage directory"))
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return codedError(ErrorCodeStorageUnavailable, "secure storage directory", err)
	}
	return nil
}

func ensureOwnerOnlyFile(path string, create bool, label string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) && create {
		f, createErr := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr != nil {
			return codedError(ErrorCodeStorageUnavailable, "create "+label, createErr)
		}
		if closeErr := f.Close(); closeErr != nil {
			return codedError(ErrorCodeStorageUnavailable, "close "+label, closeErr)
		}
		return nil
	}
	if errors.Is(err, fs.ErrNotExist) && !create {
		return nil
	}
	if err != nil {
		return codedError(ErrorCodeStorageUnavailable, "inspect "+label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return codedError(ErrorCodeStorageUnavailable, label+" is not a regular file", errors.New("unsafe storage file"))
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return codedError(ErrorCodeStorageUnavailable, "secure "+label, err)
	}
	return nil
}

func withConnectionOptions(path string, opts OpenOptions) string {
	query := url.Values{immediateTxlockParam: {"immediate"}}
	busyMS := defaultBusyTimeoutMS
	if opts.BusyTimeout > 0 {
		busyMS = int(opts.BusyTimeout / time.Millisecond)
	}
	query.Set(busyTimeoutQueryParam, strconv.Itoa(busyMS))
	return (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String()
}

// openReadOnly opens a SQLite database without ever writing to it, so a
// backup snapshot and its verification stay untouched.
func openReadOnly(ctx context.Context, path string) (*sql.DB, error) {
	if err := validateReadOnlyPath(path); err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
	db, err := sql.Open(sqlDriverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database read-only: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open sqlite database read-only: %w", err)
	}
	return db, nil
}

func validateReadOnlyPath(path string) error {
	dir := filepath.Dir(path)
	info, err := os.Lstat(dir)
	if err != nil {
		return codedError(ErrorCodeStorageUnavailable, "inspect storage directory", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return codedError(ErrorCodeStorageUnavailable, "storage directory permissions are unsafe", errors.New("owner-only storage directory required"))
	}
	if err := validateReadOnlyFile(path, "database", true); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := validateReadOnlyFile(path+suffix, "database sidecar", false); err != nil {
			return err
		}
	}
	return nil
}

func validateReadOnlyFile(path, label string, required bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if !required {
			return nil
		}
		return codedError(ErrorCodeStorageUnavailable, "read-only database is missing", err)
	}
	if err != nil {
		return codedError(ErrorCodeStorageUnavailable, "inspect "+label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return codedError(ErrorCodeStorageUnavailable, label+" permissions are unsafe", errors.New("owner-only storage file required"))
	}
	return nil
}

// OpenReadOnly opens the live database without ever writing to it: no WAL
// switch, no migration, no pragma that persists. Diagnostics surfaces use
// this so a status sweep is provably read-only. A missing file is a typed
// storage-unavailable error.
func OpenReadOnly(ctx context.Context, path string) (*sql.DB, error) {
	db, err := openReadOnly(ctx, path)
	if err != nil {
		return nil, codedError(ErrorCodeStorageUnavailable, "open sqlite database read-only", err)
	}
	return db, nil
}

func verifyConnectionPolicy(conn sqlite.ExecQuerierContext, dsn string) error {
	ctx := context.Background()
	readOnly := strings.Contains(dsn, "mode=ro")

	// busy_timeout must be applied before journal_mode: switching to WAL
	// briefly needs an exclusive lock, and without a timeout yet in place a
	// concurrently-opened connection can fail with "database is locked"
	// instead of waiting.
	pragmas := make([]struct {
		name string
		set  string
		want string
	}, 0, len(staticPragmaSet)+1)
	pragmas = append(pragmas, struct {
		name string
		set  string
		want string
	}{name: "busy_timeout", set: fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeoutFromDSN(dsn)), want: strconv.Itoa(busyTimeoutFromDSN(dsn))})
	pragmas = append(pragmas, staticPragmaSet...)

	for _, p := range pragmas {
		// journal_mode would mutate a read-only database; it is the only
		// pragma here that writes to the file.
		if readOnly && p.name == "journal_mode" {
			continue
		}
		if err := execPragma(ctx, conn, p.set); err != nil {
			return fmt.Errorf("apply pragma %s: %w", p.name, err)
		}
	}
	for _, p := range pragmas {
		if readOnly && p.name == "journal_mode" {
			continue
		}
		got, err := queryPragma(ctx, conn, p.name)
		if err != nil {
			return fmt.Errorf("verify pragma %s: %w", p.name, err)
		}
		if !strings.EqualFold(got, p.want) {
			return fmt.Errorf("pragma %s = %q, want %q", p.name, got, p.want)
		}
	}
	return nil
}

func busyTimeoutFromDSN(dsn string) int {
	u, err := url.Parse(dsn)
	if err != nil {
		return defaultBusyTimeoutMS
	}
	if raw := u.Query().Get(busyTimeoutQueryParam); raw != "" {
		if ms, err := strconv.Atoi(raw); err == nil && ms > 0 {
			return ms
		}
	}
	return defaultBusyTimeoutMS
}

func execPragma(ctx context.Context, conn sqlite.ExecQuerierContext, stmt string) error {
	rows, err := conn.QueryContext(ctx, stmt, nil)
	if err != nil {
		return err
	}
	return drainRows(rows)
}

func queryPragma(ctx context.Context, conn sqlite.ExecQuerierContext, name string) (string, error) {
	rows, err := conn.QueryContext(ctx, "PRAGMA "+name, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()

	dest := make([]driver.Value, len(rows.Columns()))
	if err := rows.Next(dest); err != nil {
		return "", fmt.Errorf("no result row for pragma %s: %w", name, err)
	}
	return fmt.Sprint(dest[0]), nil
}

func drainRows(rows driver.Rows) error {
	defer func() { _ = rows.Close() }()

	dest := make([]driver.Value, len(rows.Columns()))
	for {
		err := rows.Next(dest)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
