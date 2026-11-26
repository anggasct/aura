package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
)

func openTxTestDB(t *testing.T, opts Options) (db *sql.DB, path string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "tx.db")
	var err error
	db, err = Open(t.Context(), path, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE items (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("create items: %v", err)
	}
	return db, path
}

func countItems(ctx context.Context, t *testing.T, q Querier) int {
	t.Helper()
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM items`).Scan(&n); err != nil {
		t.Fatalf("count items: %v", err)
	}
	return n
}

func TestRunInTxCommitsWhenTheFunctionSucceeds(t *testing.T) {
	db, _ := openTxTestDB(t, Options{})
	err := RunInTx(t.Context(), db, func(txCtx context.Context) error {
		if !InTx(txCtx, db) {
			t.Fatal("InTx = false inside RunInTx")
		}
		_, err := Conn(txCtx, db).ExecContext(txCtx, `INSERT INTO items (id) VALUES ('a')`)
		return err
	})
	if err != nil {
		t.Fatalf("RunInTx: %v", err)
	}
	if got := countItems(t.Context(), t, db); got != 1 {
		t.Fatalf("items = %d, want 1", got)
	}
}

func TestRunInTxRollsBackWhenTheFunctionFails(t *testing.T) {
	db, _ := openTxTestDB(t, Options{})
	wantErr := errors.New("boom")
	err := RunInTx(t.Context(), db, func(txCtx context.Context) error {
		if _, err := Conn(txCtx, db).ExecContext(txCtx, `INSERT INTO items (id) VALUES ('a')`); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if got := countItems(t.Context(), t, db); got != 0 {
		t.Fatalf("items = %d, want 0", got)
	}
}

func TestRunInTxRollsBackWhenTheFunctionPanics(t *testing.T) {
	db, _ := openTxTestDB(t, Options{})
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = RunInTx(t.Context(), db, func(txCtx context.Context) error {
			if _, err := Conn(txCtx, db).ExecContext(txCtx, `INSERT INTO items (id) VALUES ('a')`); err != nil {
				return err
			}
			panic("tx boom")
		})
	}()
	if recovered != "tx boom" {
		t.Fatalf("recovered = %v, want %q", recovered, "tx boom")
	}
	if got := countItems(t.Context(), t, db); got != 0 {
		t.Fatalf("items = %d, want 0", got)
	}
}

func TestNestedRunInTxJoinsTheOuterTransaction(t *testing.T) {
	db, path := openTxTestDB(t, Options{BusyTimeout: 50 * time.Millisecond})
	observer, err := Open(t.Context(), path, Options{BusyTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("Open observer: %v", err)
	}
	t.Cleanup(func() { _ = observer.Close() })

	t.Run("inner success commits only when outer commits", func(t *testing.T) {
		err := RunInTx(t.Context(), db, func(outerCtx context.Context) error {
			if _, err := Conn(outerCtx, db).ExecContext(outerCtx, `INSERT INTO items (id) VALUES ('outer')`); err != nil {
				return err
			}
			if err := RunInTx(outerCtx, db, func(innerCtx context.Context) error {
				_, err := Conn(innerCtx, db).ExecContext(innerCtx, `INSERT INTO items (id) VALUES ('inner')`)
				return err
			}); err != nil {
				return err
			}
			if got := countItems(outerCtx, t, observer); got != 0 {
				t.Fatalf("observer saw %d items before outer commit, want 0", got)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("RunInTx: %v", err)
		}
		if got := countItems(t.Context(), t, observer); got != 2 {
			t.Fatalf("observer saw %d items after outer commit, want 2", got)
		}
	})

	t.Run("inner error rolls back outer writes", func(t *testing.T) {
		innerErr := errors.New("inner failed")
		err := RunInTx(t.Context(), db, func(outerCtx context.Context) error {
			if _, err := Conn(outerCtx, db).ExecContext(outerCtx, `INSERT INTO items (id) VALUES ('outer-2')`); err != nil {
				return err
			}
			return RunInTx(outerCtx, db, func(innerCtx context.Context) error {
				if _, err := Conn(innerCtx, db).ExecContext(innerCtx, `INSERT INTO items (id) VALUES ('inner-2')`); err != nil {
					return err
				}
				return innerErr
			})
		})
		if !errors.Is(err, innerErr) {
			t.Fatalf("err = %v, want %v", err, innerErr)
		}
		if got := countItems(t.Context(), t, observer); got != 2 {
			t.Fatalf("observer saw %d items after rollback, want 2", got)
		}
	})
}

func TestNestedRunInTxErrorMarksTheTransactionRollbackOnly(t *testing.T) {
	sentinel := errors.New("dry run stop")
	cases := []struct {
		name     string
		innerErr error
	}{
		{"regular error", errors.New("nested failure")},
		{"sentinel error", sentinel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := openTxTestDB(t, Options{})
			err := RunInTx(t.Context(), db, func(outerCtx context.Context) error {
				if _, err := Conn(outerCtx, db).ExecContext(outerCtx, `INSERT INTO items (id) VALUES ('outer')`); err != nil {
					return err
				}
				nestedErr := RunInTx(outerCtx, db, func(innerCtx context.Context) error {
					if _, err := Conn(innerCtx, db).ExecContext(innerCtx, `INSERT INTO items (id) VALUES ('inner')`); err != nil {
						return err
					}
					return tc.innerErr
				})
				if !errors.Is(nestedErr, tc.innerErr) {
					t.Fatalf("nested err = %v, want %v", nestedErr, tc.innerErr)
				}
				return nil
			})
			if !fault.HasCode(err, ErrorCodeFailed) {
				t.Fatalf("err = %v, want code %s", err, ErrorCodeFailed)
			}
			const wantText = "storage_failed: commit transaction: rolled back by a nested call"
			if err.Error() != wantText {
				t.Fatalf("err = %q, want %q", err.Error(), wantText)
			}
			if got := countItems(t.Context(), t, db); got != 0 {
				t.Fatalf("items = %d, want 0", got)
			}
		})
	}
}

func TestRunInTxNeverJoinsAnotherDatabase(t *testing.T) {
	dbA, _ := openTxTestDB(t, Options{})
	dbB, _ := openTxTestDB(t, Options{})

	rollbackA := errors.New("rollback A")
	err := RunInTx(t.Context(), dbA, func(ctxA context.Context) error {
		if !InTx(ctxA, dbA) {
			t.Fatal("InTx(ctxA, dbA) = false, want true")
		}
		if InTx(ctxA, dbB) {
			t.Fatal("InTx(ctxA, dbB) = true, want false")
		}
		if Conn(ctxA, dbB) != dbB {
			t.Fatal("Conn(ctxA, dbB) did not return dbB")
		}
		if _, err := Conn(ctxA, dbA).ExecContext(ctxA, `INSERT INTO items (id) VALUES ('a1')`); err != nil {
			return err
		}
		if err := RunInTx(ctxA, dbB, func(ctxAB context.Context) error {
			if !InTx(ctxAB, dbA) || !InTx(ctxAB, dbB) {
				t.Fatalf("InTx(A)=%v InTx(B)=%v, want both true", InTx(ctxAB, dbA), InTx(ctxAB, dbB))
			}
			if Conn(ctxAB, dbA) == dbA {
				t.Fatal("Conn(ctxAB, dbA) lost database A's transaction")
			}
			if Conn(ctxAB, dbB) == dbB {
				t.Fatal("Conn(ctxAB, dbB) did not return database B's transaction")
			}
			if _, err := Conn(ctxAB, dbA).ExecContext(ctxAB, `INSERT INTO items (id) VALUES ('a2')`); err != nil {
				return err
			}
			_, err := Conn(ctxAB, dbB).ExecContext(ctxAB, `INSERT INTO items (id) VALUES ('b1')`)
			return err
		}); err != nil {
			return err
		}
		return rollbackA
	})
	if !errors.Is(err, rollbackA) {
		t.Fatalf("err = %v, want %v", err, rollbackA)
	}
	if got := countItems(t.Context(), t, dbA); got != 0 {
		t.Fatalf("dbA items = %d, want 0", got)
	}
	if got := countItems(t.Context(), t, dbB); got != 1 {
		t.Fatalf("dbB items = %d, want 1", got)
	}
}

func TestConnOutsideATransactionIsTheDatabase(t *testing.T) {
	db, _ := openTxTestDB(t, Options{})
	if InTx(t.Context(), db) {
		t.Fatal("InTx = true outside transaction")
	}
	if Conn(t.Context(), db) != db {
		t.Fatal("Conn did not return db outside transaction")
	}
	if InTx(t.Context(), nil) {
		t.Fatal("InTx(nil) = true")
	}
	if Conn(t.Context(), nil) != nil {
		t.Fatal("Conn(nil) != nil")
	}
}

func TestRunInTxRefusesNilArguments(t *testing.T) {
	db, _ := openTxTestDB(t, Options{})
	cases := []struct {
		name string
		db   *sql.DB
		fn   func(context.Context) error
	}{
		{"nil db", nil, func(context.Context) error { return nil }},
		{"nil fn", db, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := RunInTx(t.Context(), tc.db, tc.fn)
			if !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("err = %v, want code %s", err, ErrorCodeInvalidArgument)
			}
		})
	}
}

func TestRunInTxClassifiesBeginAndCommitFailures(t *testing.T) {
	t.Run("busy begin", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "busy.db")
		holder, err := Open(t.Context(), path, Options{BusyTimeout: time.Second})
		if err != nil {
			t.Fatalf("Open holder: %v", err)
		}
		t.Cleanup(func() { _ = holder.Close() })
		if _, err := holder.ExecContext(t.Context(), `CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
			t.Fatalf("create table: %v", err)
		}
		tx, err := holder.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatalf("holder BeginTx: %v", err)
		}
		t.Cleanup(func() { _ = tx.Rollback() })

		contender, err := Open(t.Context(), path, Options{BusyTimeout: time.Millisecond})
		if err != nil {
			t.Fatalf("Open contender: %v", err)
		}
		t.Cleanup(func() { _ = contender.Close() })

		err = RunInTx(t.Context(), contender, func(context.Context) error { return nil })
		if !fault.HasCode(err, ErrorCodeBusy) {
			t.Fatalf("err = %v, want code %s", err, ErrorCodeBusy)
		}
		if !strings.HasPrefix(err.Error(), "storage_busy: begin transaction:") {
			t.Fatalf("err = %q, want prefix %q", err.Error(), "storage_busy: begin transaction:")
		}
	})

	t.Run("closed database begin", func(t *testing.T) {
		db, _ := openTxTestDB(t, Options{})
		if err := db.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		err := RunInTx(t.Context(), db, func(context.Context) error { return nil })
		if !fault.HasCode(err, ErrorCodeFailed) {
			t.Fatalf("err = %v, want code %s", err, ErrorCodeFailed)
		}
		if !strings.HasPrefix(err.Error(), "storage_failed: begin transaction:") {
			t.Fatalf("err = %q, want prefix %q", err.Error(), "storage_failed: begin transaction:")
		}
	})

	t.Run("cancelled context begin preserves cause", func(t *testing.T) {
		db, _ := openTxTestDB(t, Options{})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := RunInTx(ctx, db, func(context.Context) error { return nil })
		if !fault.HasCode(err, ErrorCodeFailed) {
			t.Fatalf("err = %v, want code %s", err, ErrorCodeFailed)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want errors.Is(context.Canceled)", err)
		}
	})

	t.Run("deferred foreign key commit failure", func(t *testing.T) {
		db, _ := openTxTestDB(t, Options{})
		if _, err := db.ExecContext(t.Context(), `
CREATE TABLE parent (id TEXT PRIMARY KEY);
CREATE TABLE child (id TEXT PRIMARY KEY, parent_id TEXT NOT NULL REFERENCES parent(id));
`); err != nil {
			t.Fatalf("create tables: %v", err)
		}
		err := RunInTx(t.Context(), db, func(txCtx context.Context) error {
			if _, err := Conn(txCtx, db).ExecContext(txCtx, `PRAGMA defer_foreign_keys = ON`); err != nil {
				return err
			}
			_, err := Conn(txCtx, db).ExecContext(txCtx, `INSERT INTO child (id, parent_id) VALUES ('c1', 'missing')`)
			return err
		})
		if !fault.HasCode(err, ErrorCodeConstraintForeignKey) {
			t.Fatalf("err = %v, want code %s", err, ErrorCodeConstraintForeignKey)
		}
		if !strings.HasPrefix(err.Error(), "storage_constraint_foreign_key: commit transaction:") {
			t.Fatalf("err = %q, want prefix %q", err.Error(), "storage_constraint_foreign_key: commit transaction:")
		}
	})
}

func TestAfterCommitRunsOnlyAfterTheOutermostCommit(t *testing.T) {
	db, _ := openTxTestDB(t, Options{})

	t.Run("outside transaction runs immediately", func(t *testing.T) {
		ran := false
		AfterCommit(t.Context(), db, nil)
		AfterCommit(t.Context(), db, func() { ran = true })
		if !ran {
			t.Fatal("hook did not run immediately outside transaction")
		}
	})

	t.Run("nested hooks run in order only after outermost commit", func(t *testing.T) {
		var order []string
		err := RunInTx(t.Context(), db, func(outerCtx context.Context) error {
			AfterCommit(outerCtx, db, func() { order = append(order, "outer-1") })
			if err := RunInTx(outerCtx, db, func(innerCtx context.Context) error {
				AfterCommit(innerCtx, db, func() { order = append(order, "inner") })
				return nil
			}); err != nil {
				return err
			}
			if len(order) != 0 {
				t.Fatalf("hooks ran before outermost commit: %v", order)
			}
			AfterCommit(outerCtx, db, func() { order = append(order, "outer-2") })
			return nil
		})
		if err != nil {
			t.Fatalf("RunInTx: %v", err)
		}
		want := []string{"outer-1", "inner", "outer-2"}
		if !slices.Equal(order, want) {
			t.Fatalf("order = %v, want %v", order, want)
		}
	})

	t.Run("rollback drops hooks", func(t *testing.T) {
		ran := false
		_ = RunInTx(t.Context(), db, func(txCtx context.Context) error {
			AfterCommit(txCtx, db, func() { ran = true })
			return errors.New("abort")
		})
		if ran {
			t.Fatal("hook ran after rollback")
		}
	})

	t.Run("rollback-only drops hooks", func(t *testing.T) {
		ran := false
		err := RunInTx(t.Context(), db, func(outerCtx context.Context) error {
			AfterCommit(outerCtx, db, func() { ran = true })
			_ = RunInTx(outerCtx, db, func(innerCtx context.Context) error {
				AfterCommit(innerCtx, db, func() { ran = true })
				return errors.New("nested fail")
			})
			return nil
		})
		if !fault.HasCode(err, ErrorCodeFailed) {
			t.Fatalf("err = %v, want code %s", err, ErrorCodeFailed)
		}
		if ran {
			t.Fatal("hook ran after rollback-only")
		}
	})
}
