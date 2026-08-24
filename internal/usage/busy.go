package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"modernc.org/sqlite"
)

// sqliteBusyCode is the driver's SQLITE_BUSY result code: another connection
// held the write lock longer than the connection's busy_timeout.
const sqliteBusyCode = 5

func isTransientBusy(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == sqliteBusyCode
}

// The DSN pins transactions to BEGIN IMMEDIATE, so BeginTx queues for the
// write lock and fails with SQLITE_BUSY only after the connection's whole
// busy_timeout has elapsed. A burst of concurrent reserves therefore
// serializes past one timeout window on slow disks even though each
// transaction is short; a small number of retried attempts absorbs that tail
// while the caller's context stays the hard bound.
const (
	busyMaxAttempts = 4
	busyBaseDelay   = 25 * time.Millisecond
	busyMaxDelay    = 500 * time.Millisecond
)

// beginner is the transaction-entry surface of *sql.DB.
type beginner interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// beginTx begins a write transaction, retrying transient SQLITE_BUSY with
// bounded jittered backoff.
func beginTx(ctx context.Context, db beginner, operation string) (*sql.Tx, error) {
	delay := busyBaseDelay
	for attempt := 1; ; attempt++ {
		tx, err := db.BeginTx(ctx, nil)
		if err == nil {
			return tx, nil
		}
		if !isTransientBusy(err) || attempt >= busyMaxAttempts {
			return nil, fmt.Errorf("usage: begin %s transaction: %w", operation, err)
		}
		timer := time.NewTimer(delay + rand.N(delay/2))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("usage: begin %s transaction: %w", operation, ctx.Err())
		case <-timer.C:
		}
		delay = min(delay*2, busyMaxDelay)
	}
}
