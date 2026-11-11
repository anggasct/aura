package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/anggasct/aura/internal/platform/durable"
)

const dbNowMS = "CAST(unixepoch('subsec') * 1000 AS INTEGER)"

var errNotOwned = errors.New("durable run is not owned by this invocation")

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type signalRow struct {
	runKey  string
	name    string
	payload []byte
	stamp   string
}

type signalRef struct {
	runKey string
	name   string
	seq    int64
}

func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
}

func bytesOrEmpty(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

func copyBytes(b []byte) []byte {
	return append([]byte{}, b...)
}

func (r *Runtime) inTx(ctx context.Context, op string, fn func(tx *sql.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: begin: %w", op, err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", op, err)
	}
	return nil
}

func runExists(ctx context.Context, q querier, key string) (bool, error) {
	var exists bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM durable_run WHERE key = ?)`, key).Scan(&exists); err != nil {
		return false, fmt.Errorf("read run: %w", err)
	}
	return exists, nil
}

func requireRunning(ctx context.Context, q querier, key string) error {
	var state string
	err := q.QueryRowContext(ctx, `SELECT state FROM durable_run WHERE key = ?`, key).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return unknownRun(key)
	}
	if err != nil {
		return fmt.Errorf("read run state: %w", err)
	}
	if durable.RunState(state) != durable.RunRunning {
		return &Error{Code: ErrorCodeRunTerminal, Detail: "durable run " + key + " is " + state + " and cannot receive signals"}
	}
	return nil
}

func appendSignal(ctx context.Context, tx *sql.Tx, sig *signalRow) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO durable_signal (run_key, name, seq, payload, created_at)
SELECT ?, ?, COALESCE(MAX(seq) + 1, 0), ?, ? FROM durable_signal WHERE run_key = ? AND name = ?`,
		sig.runKey, sig.name, bytesOrEmpty(sig.payload), sig.stamp, sig.runKey, sig.name); err != nil {
		return fmt.Errorf("append signal: %w", err)
	}
	return nil
}

func readSignal(ctx context.Context, q querier, ref signalRef) (payload []byte, found bool, err error) {
	err = q.QueryRowContext(ctx, `SELECT payload FROM durable_signal WHERE run_key = ? AND name = ? AND seq = ?`, ref.runKey, ref.name, ref.seq).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read signal: %w", err)
	}
	return copyBytes(payload), true, nil
}
