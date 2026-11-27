package sqlite

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

	sqlitedriver "modernc.org/sqlite"

	"github.com/anggasct/aura/internal/platform/fault"
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
	{name: "foreign_keys", set: "PRAGMA foreign_keys = ON", want: "1"},
	{name: "journal_mode", set: "PRAGMA journal_mode = WAL", want: "wal"},
	{name: "synchronous", set: "PRAGMA synchronous = NORMAL", want: "1"},
}

type Options struct {
	BusyTimeout        time.Duration
	MaxOpenConnections int
}

func Open(ctx context.Context, path string, opts Options) (*sql.DB, error) {
	registerConnectionPolicyOnce.Do(func() {
		sqlitedriver.RegisterConnectionHook(verifyConnectionPolicy)
	})
	if path != "" && path != ":memory:" {
		if err := ensureOwnerOnly(path); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open(sqlDriverName, withConnectionOptions(path, opts))
	if err != nil {
		return nil, unavailable("open sqlite database", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, unavailable("open sqlite database", err)
	}
	if opts.MaxOpenConnections > 0 {
		db.SetMaxOpenConns(opts.MaxOpenConnections)
	}
	return db, nil
}

func unavailable(detail string, cause error) error {
	return fault.Wrap(ErrorCodeUnavailable, detail, cause)
}

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
		return unavailable("create storage directory", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return unavailable("inspect storage directory", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return unavailable("storage path is not a directory", errors.New("unsafe storage directory"))
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return unavailable("secure storage directory", err)
	}
	return nil
}

func ensureOwnerOnlyFile(path string, create bool, label string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) && create {
		f, createErr := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr != nil {
			return unavailable("create "+label, createErr)
		}
		if closeErr := f.Close(); closeErr != nil {
			return unavailable("close "+label, closeErr)
		}
		return nil
	}
	if errors.Is(err, fs.ErrNotExist) && !create {
		return nil
	}
	if err != nil {
		return unavailable("inspect "+label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return unavailable(label+" is not a regular file", errors.New("unsafe storage file"))
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return unavailable("secure "+label, err)
	}
	return nil
}

func withConnectionOptions(path string, opts Options) string {
	query := url.Values{immediateTxlockParam: {"immediate"}}
	busyMS := defaultBusyTimeoutMS
	if opts.BusyTimeout > 0 {
		busyMS = int(opts.BusyTimeout / time.Millisecond)
	}
	query.Set(busyTimeoutQueryParam, strconv.Itoa(busyMS))
	if path == ":memory:" {
		query.Set("mode", "memory")
		return "file::memory:?" + query.Encode()
	}
	return (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String()
}

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
		return unavailable("inspect storage directory", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return unavailable("storage directory permissions are unsafe", errors.New("owner-only storage directory required"))
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
		return unavailable("read-only database is missing", err)
	}
	if err != nil {
		return unavailable("inspect "+label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return unavailable(label+" permissions are unsafe", errors.New("owner-only storage file required"))
	}
	return nil
}

func OpenReadOnly(ctx context.Context, path string) (*sql.DB, error) {
	db, err := openReadOnly(ctx, path)
	if err != nil {
		return nil, unavailable("open sqlite database read-only", err)
	}
	return db, nil
}

func verifyConnectionPolicy(conn sqlitedriver.ExecQuerierContext, dsn string) error {
	ctx := context.Background()
	query := dsnQuery(dsn)
	if query.Get("immutable") == "1" {
		return nil
	}
	readOnly := query.Get("mode") == "ro"
	inMemory := dsn == ":memory:" || query.Get("mode") == "memory"
	busyTimeout := busyTimeoutFromQuery(query)

	pragmas := make([]struct {
		name string
		set  string
		want string
	}, 0, len(staticPragmaSet)+1)
	pragmas = append(pragmas, struct {
		name string
		set  string
		want string
	}{name: "busy_timeout", set: fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeout), want: strconv.Itoa(busyTimeout)})
	pragmas = append(pragmas, staticPragmaSet...)

	for _, p := range pragmas {
		if readOnly && (p.name == "journal_mode" || p.name == "synchronous") {
			continue
		}
		if inMemory && p.name == "journal_mode" {
			continue
		}
		if err := execPragma(ctx, conn, p.set); err != nil {
			return fmt.Errorf("apply pragma %s: %w", p.name, err)
		}
	}
	for _, p := range pragmas {
		if readOnly && (p.name == "journal_mode" || p.name == "synchronous") {
			continue
		}
		if inMemory && p.name == "journal_mode" {
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

func dsnQuery(dsn string) url.Values {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil
	}
	return u.Query()
}

func busyTimeoutFromQuery(query url.Values) int {
	if raw := query.Get(busyTimeoutQueryParam); raw != "" {
		if ms, err := strconv.Atoi(raw); err == nil && ms > 0 {
			return ms
		}
	}
	return defaultBusyTimeoutMS
}

func execPragma(ctx context.Context, conn sqlitedriver.ExecQuerierContext, stmt string) error {
	rows, err := conn.QueryContext(ctx, stmt, nil)
	if err != nil {
		return err
	}
	return drainRows(rows)
}

func queryPragma(ctx context.Context, conn sqlitedriver.ExecQuerierContext, name string) (string, error) {
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
