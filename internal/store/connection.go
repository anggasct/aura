package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"modernc.org/sqlite"
)

const sqlDriverName = "sqlite"

var registerConnectionPolicyOnce sync.Once

var connectionPragmaSet = []struct {
	name string
	set  string
	want string
}{
	// busy_timeout must be set before journal_mode: switching to WAL briefly
	// needs an exclusive lock, and without a timeout yet in place a
	// concurrently-opened connection can fail with "database is locked"
	// instead of waiting.
	{name: "busy_timeout", set: "PRAGMA busy_timeout = 5000", want: "5000"},
	{name: "foreign_keys", set: "PRAGMA foreign_keys = ON", want: "1"},
	{name: "journal_mode", set: "PRAGMA journal_mode = WAL", want: "wal"},
	{name: "synchronous", set: "PRAGMA synchronous = NORMAL", want: "1"},
}

// OpenDB opens a SQLite database and installs a connection policy hook so
// that every physical connection the pool creates - not just the first -
// gets foreign keys, WAL, busy timeout, and synchronous mode applied and
// verified before it is handed to a caller.
func OpenDB(ctx context.Context, dsn string) (*sql.DB, error) {
	registerConnectionPolicyOnce.Do(func() {
		sqlite.RegisterConnectionHook(verifyConnectionPolicy)
	})

	db, err := sql.Open(sqlDriverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	return db, nil
}

func verifyConnectionPolicy(conn sqlite.ExecQuerierContext, _ string) error {
	ctx := context.Background()
	for _, p := range connectionPragmaSet {
		if err := execPragma(ctx, conn, p.set); err != nil {
			return fmt.Errorf("apply pragma %s: %w", p.name, err)
		}
	}
	for _, p := range connectionPragmaSet {
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
