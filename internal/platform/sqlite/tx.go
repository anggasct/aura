package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"maps"

	"github.com/anggasct/aura/internal/platform/fault"
)

type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
}

type txContextKey struct{}

type txState struct {
	tx           *sql.Tx
	rollbackOnly bool
	afterCommit  []func()
}

var errRolledBackByNestedCall = errors.New("rolled back by a nested call")

func RunInTx(ctx context.Context, db *sql.DB, fn func(ctx context.Context) error) error {
	if db == nil {
		return fault.New(ErrorCodeInvalidArgument, "database is required")
	}
	if fn == nil {
		return fault.New(ErrorCodeInvalidArgument, "transaction function is required")
	}
	if state := txFor(ctx, db); state != nil {
		return runJoinedTx(ctx, state, fn)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return wrapTxError("begin transaction", err)
	}
	state := &txState{tx: tx}
	txCtx := withTx(ctx, db, state)
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := fn(txCtx); err != nil {
		return err
	}
	if state.rollbackOnly {
		return fault.Wrap(ErrorCodeFailed, "commit transaction", errRolledBackByNestedCall)
	}
	if err := tx.Commit(); err != nil {
		return wrapTxError("commit transaction", err)
	}
	committed = true
	for _, hook := range state.afterCommit {
		hook()
	}
	return nil
}

func Conn(ctx context.Context, db *sql.DB) Querier {
	if db == nil {
		return nil
	}
	if state := txFor(ctx, db); state != nil {
		return state.tx
	}
	return db
}

func InTx(ctx context.Context, db *sql.DB) bool {
	return txFor(ctx, db) != nil
}

func AfterCommit(ctx context.Context, db *sql.DB, fn func()) {
	if fn == nil {
		return
	}
	if state := txFor(ctx, db); state != nil {
		state.afterCommit = append(state.afterCommit, fn)
		return
	}
	fn()
}

func runJoinedTx(ctx context.Context, state *txState, fn func(ctx context.Context) error) (err error) {
	completed := false
	defer func() {
		if !completed || err != nil {
			state.rollbackOnly = true
		}
	}()
	err = fn(ctx)
	completed = true
	return err
}

func txFor(ctx context.Context, db *sql.DB) *txState {
	if ctx == nil || db == nil {
		return nil
	}
	entries, ok := ctx.Value(txContextKey{}).(map[*sql.DB]*txState)
	if !ok {
		return nil
	}
	return entries[db]
}

func withTx(ctx context.Context, db *sql.DB, state *txState) context.Context {
	existing, _ := ctx.Value(txContextKey{}).(map[*sql.DB]*txState)
	next := make(map[*sql.DB]*txState, len(existing)+1)
	maps.Copy(next, existing)
	next[db] = state
	return context.WithValue(ctx, txContextKey{}, next)
}

func wrapTxError(detail string, err error) error {
	return fault.Wrap(errorCode(err), detail, err)
}
