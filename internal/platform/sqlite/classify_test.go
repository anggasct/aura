package sqlite

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sqlitedriver "modernc.org/sqlite"

	"github.com/anggasct/aura/internal/platform/fault"
)

func TestClassifyMapsDriverResultCodes(t *testing.T) {
	cases := []struct {
		name         string
		produce      func(t *testing.T) error
		wantCode     ErrorCode
		wantRawCode  int
		wantTextPref string
	}{
		{
			name: "unique constraint",
			produce: func(t *testing.T) error {
				t.Helper()
				db := openFresh(t, Options{})
				if _, err := db.ExecContext(t.Context(), `CREATE TABLE u (id INTEGER PRIMARY KEY, k TEXT UNIQUE)`); err != nil {
					t.Fatalf("create table: %v", err)
				}
				if _, err := db.ExecContext(t.Context(), `INSERT INTO u (id, k) VALUES (1, 'a')`); err != nil {
					t.Fatalf("seed row: %v", err)
				}
				_, err := db.ExecContext(t.Context(), `INSERT INTO u (id, k) VALUES (2, 'a')`)
				return err
			},
			wantCode:    ErrorCodeConstraintUnique,
			wantRawCode: sqliteConstraintUnique,
		},
		{
			name: "primary key constraint",
			produce: func(t *testing.T) error {
				t.Helper()
				db := openFresh(t, Options{})
				if _, err := db.ExecContext(t.Context(), `CREATE TABLE pk (id TEXT PRIMARY KEY)`); err != nil {
					t.Fatalf("create table: %v", err)
				}
				if _, err := db.ExecContext(t.Context(), `INSERT INTO pk (id) VALUES ('a')`); err != nil {
					t.Fatalf("seed row: %v", err)
				}
				_, err := db.ExecContext(t.Context(), `INSERT INTO pk (id) VALUES ('a')`)
				return err
			},
			wantCode:    ErrorCodeConstraintPrimaryKey,
			wantRawCode: sqliteConstraintPrimaryKey,
		},
		{
			name: "foreign key constraint",
			produce: func(t *testing.T) error {
				t.Helper()
				db := openFresh(t, Options{})
				if _, err := db.ExecContext(t.Context(), `CREATE TABLE parent (id TEXT PRIMARY KEY); CREATE TABLE child (id TEXT PRIMARY KEY, parent_id TEXT NOT NULL REFERENCES parent(id))`); err != nil {
					t.Fatalf("create tables: %v", err)
				}
				_, err := db.ExecContext(t.Context(), `INSERT INTO child (id, parent_id) VALUES ('c1', 'missing')`)
				return err
			},
			wantCode:    ErrorCodeConstraintForeignKey,
			wantRawCode: sqliteConstraintForeignKey,
		},
		{
			name: "check constraint",
			produce: func(t *testing.T) error {
				t.Helper()
				db := openFresh(t, Options{})
				if _, err := db.ExecContext(t.Context(), `CREATE TABLE chk (n INTEGER CHECK(n > 0))`); err != nil {
					t.Fatalf("create table: %v", err)
				}
				_, err := db.ExecContext(t.Context(), `INSERT INTO chk (n) VALUES (0)`)
				return err
			},
			wantCode:    ErrorCodeConstraintCheck,
			wantRawCode: sqliteConstraintCheck,
		},
		{
			name: "not null constraint",
			produce: func(t *testing.T) error {
				t.Helper()
				db := openFresh(t, Options{})
				if _, err := db.ExecContext(t.Context(), `CREATE TABLE nn (v TEXT NOT NULL)`); err != nil {
					t.Fatalf("create table: %v", err)
				}
				_, err := db.ExecContext(t.Context(), `INSERT INTO nn (v) VALUES (NULL)`)
				return err
			},
			wantCode:    ErrorCodeConstraintNotNull,
			wantRawCode: sqliteConstraintNotNull,
		},
		{
			name: "trigger raise abort",
			produce: func(t *testing.T) error {
				t.Helper()
				db := openFresh(t, Options{})
				if _, err := db.ExecContext(t.Context(), `
CREATE TABLE trg (v TEXT);
CREATE TRIGGER trg_abort BEFORE INSERT ON trg BEGIN SELECT RAISE(ABORT, 'blocked'); END;
`); err != nil {
					t.Fatalf("create trigger: %v", err)
				}
				_, err := db.ExecContext(t.Context(), `INSERT INTO trg (v) VALUES ('x')`)
				return err
			},
			wantCode: ErrorCodeConstraint,
		},
		{
			name: "busy snapshot",
			produce: func(t *testing.T) error {
				t.Helper()
				path := filepath.Join(t.TempDir(), "snapshot.db")
				writer, err := Open(t.Context(), path, Options{BusyTimeout: time.Millisecond})
				if err != nil {
					t.Fatalf("open writer: %v", err)
				}
				t.Cleanup(func() { _ = writer.Close() })
				if _, err := writer.ExecContext(t.Context(), `CREATE TABLE items (id INTEGER PRIMARY KEY)`); err != nil {
					t.Fatalf("create table: %v", err)
				}
				reader, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_busy_timeout=1")
				if err != nil {
					t.Fatalf("open reader: %v", err)
				}
				t.Cleanup(func() { _ = reader.Close() })
				tx, err := reader.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatalf("begin deferred tx: %v", err)
				}
				t.Cleanup(func() { _ = tx.Rollback() })
				var count int
				if err := tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM items`).Scan(&count); err != nil {
					t.Fatalf("read snapshot: %v", err)
				}
				if _, err := writer.ExecContext(t.Context(), `INSERT INTO items (id) VALUES (1)`); err != nil {
					t.Fatalf("concurrent write: %v", err)
				}
				_, err = tx.ExecContext(t.Context(), `INSERT INTO items (id) VALUES (2)`)
				return err
			},
			wantCode:     ErrorCodeBusy,
			wantRawCode:  517,
			wantTextPref: "storage_busy: database is busy: ",
		},
		{
			name: "busy lock timeout",
			produce: func(t *testing.T) error {
				t.Helper()
				path := filepath.Join(t.TempDir(), "busy.db")
				holder, err := Open(t.Context(), path, Options{BusyTimeout: time.Millisecond})
				if err != nil {
					t.Fatalf("open holder: %v", err)
				}
				t.Cleanup(func() { _ = holder.Close() })
				if _, err := holder.ExecContext(t.Context(), `CREATE TABLE items (id INTEGER PRIMARY KEY)`); err != nil {
					t.Fatalf("create table: %v", err)
				}
				waiter, err := Open(t.Context(), path, Options{BusyTimeout: time.Millisecond})
				if err != nil {
					t.Fatalf("open waiter: %v", err)
				}
				t.Cleanup(func() { _ = waiter.Close() })
				tx, err := holder.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatalf("begin holder tx: %v", err)
				}
				t.Cleanup(func() { _ = tx.Rollback() })
				_, err = waiter.ExecContext(t.Context(), `INSERT INTO items (id) VALUES (1)`)
				return err
			},
			wantCode:     ErrorCodeBusy,
			wantRawCode:  sqliteBusy,
			wantTextPref: "storage_busy: database is busy: ",
		},
		{
			name: "not a database",
			produce: func(t *testing.T) error {
				t.Helper()
				path := filepath.Join(t.TempDir(), "notadb.db")
				if err := os.WriteFile(path, []byte("this is definitely not a sqlite database file"), 0o600); err != nil {
					t.Fatalf("write bogus file: %v", err)
				}
				db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
				if err != nil {
					t.Fatalf("open bogus file: %v", err)
				}
				t.Cleanup(func() { _ = db.Close() })
				_, err = db.ExecContext(t.Context(), `SELECT 1 FROM sqlite_schema`)
				return err
			},
			wantCode:    ErrorCodeUnavailable,
			wantRawCode: sqliteNotADB,
		},
		{
			name: "syntax error",
			produce: func(t *testing.T) error {
				t.Helper()
				db := openFresh(t, Options{})
				_, err := db.ExecContext(t.Context(), `SELEC BROKEN SQL`)
				return err
			},
			wantCode: ErrorCodeFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.produce(t)
			if raw == nil {
				t.Fatal("expected driver error, got nil")
			}
			var driverErr *sqlitedriver.Error
			if !errors.As(raw, &driverErr) {
				t.Fatalf("expected *sqlitedriver.Error, got %T: %v", raw, raw)
			}
			if tc.wantRawCode != 0 && driverErr.Code() != tc.wantRawCode {
				t.Fatalf("driver code = %d, want %d (%v)", driverErr.Code(), tc.wantRawCode, raw)
			}
			got := Classify(raw)
			if !fault.HasCode(got, tc.wantCode) {
				t.Fatalf("Classify(%v) = %v, want code %s", raw, got, tc.wantCode)
			}
			if tc.wantTextPref != "" && !strings.HasPrefix(got.Error(), tc.wantTextPref) {
				t.Fatalf("error text = %q, want prefix %q", got.Error(), tc.wantTextPref)
			}
		})
	}
}

func TestClassifyKeepsTheDriverErrorAndIsIdempotent(t *testing.T) {
	if got := Classify(nil); got != nil {
		t.Fatalf("Classify(nil) = %v, want nil", got)
	}
	plain := errors.New("plain failure")
	if got := Classify(plain); !errors.Is(got, plain) || got.Error() != plain.Error() {
		t.Fatalf("Classify(plain) = %v, want unchanged %v", got, plain)
	}

	db := openFresh(t, Options{})
	_, raw := db.ExecContext(t.Context(), `SELEC BROKEN SQL`)
	if raw == nil {
		t.Fatal("expected syntax error, got nil")
	}
	first := Classify(raw)
	var driverErr *sqlitedriver.Error
	if !errors.As(first, &driverErr) {
		t.Fatalf("classified error dropped *sqlitedriver.Error: %v", first)
	}
	second := Classify(first)
	if !errors.Is(second, first) || second.Error() != first.Error() {
		t.Fatalf("Classify(Classify(err)) = %q, want %q", second.Error(), first.Error())
	}
}

func TestMigrateClassifiesAFailingMigration(t *testing.T) {
	db := openFresh(t, Options{})
	err := Migrate(t.Context(), db, []Migration{
		{Version: 1, Owner: "test", SQL: `CREATE TABLE t1 (id INTEGER PRIMARY KEY);`},
		{Version: 2, Owner: "test", SQL: `INVALID SQL STATEMENT;`},
	})
	if err == nil {
		t.Fatal("expected migration error, got nil")
	}
	if !fault.HasCode(err, ErrorCodeFailed) {
		t.Fatalf("Migrate error = %v, want code %s", err, ErrorCodeFailed)
	}
	if !strings.Contains(err.Error(), "2") {
		t.Fatalf("Migrate error %q does not mention failing version 2", err.Error())
	}
}
