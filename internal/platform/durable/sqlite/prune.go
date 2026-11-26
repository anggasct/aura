package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	dbsqlite "github.com/anggasct/aura/internal/platform/sqlite"
)

const pruneSecondLayout = "2006-01-02T15:04:05"

const pruneEndedSQL = `DELETE FROM durable_run WHERE key IN (
    SELECT key FROM durable_run
    WHERE state <> 'running'
      AND handler IN (SELECT value FROM json_each(?))
      AND substr(updated_at, 1, 19) < ?
      AND key NOT IN (SELECT turn_id FROM durable_session_turn)
    ORDER BY substr(updated_at, 1, 19), rowid
    LIMIT ?)`

type PruneRequest struct {
	Handlers []string
	Before   time.Time
	Limit    int
}

func (r *Runtime) PruneEnded(ctx context.Context, req PruneRequest) (int, error) {
	if err := validatePrune(&req); err != nil {
		return 0, err
	}
	names, err := json.Marshal(req.Handlers)
	if err != nil {
		return 0, fmt.Errorf("durable prune: encode handler names: %w", err)
	}
	res, err := dbsqlite.Conn(ctx, r.db).ExecContext(ctx, pruneEndedSQL, string(names), req.Before.UTC().Format(pruneSecondLayout), req.Limit)
	if err != nil {
		return 0, fmt.Errorf("durable prune: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("durable prune: %w", err)
	}
	return int(deleted), nil
}

func validatePrune(req *PruneRequest) error {
	var problems []error
	if len(req.Handlers) == 0 {
		problems = append(problems, invalidArgument("durable prune requires at least one handler"))
	}
	if req.Before.IsZero() {
		problems = append(problems, invalidArgument("durable prune requires a cutoff"))
	}
	if req.Limit <= 0 {
		problems = append(problems, invalidArgument("durable prune limit must be positive"))
	}
	return errors.Join(problems...)
}

func (r *Runtime) RunsOf(ctx context.Context, handler string) ([]durable.HandlerRun, error) {
	rows, err := dbsqlite.Conn(ctx, r.db).QueryContext(ctx,
		`SELECT key, payload FROM durable_run WHERE handler = ? ORDER BY key`, handler,
	)
	if err != nil {
		return nil, dbsqlite.Classify(fmt.Errorf("durable list runs: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var runs []durable.HandlerRun
	for rows.Next() {
		var run durable.HandlerRun
		if err := rows.Scan(&run.Key, &run.Payload); err != nil {
			return nil, dbsqlite.Classify(fmt.Errorf("durable list runs: %w", err))
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, dbsqlite.Classify(fmt.Errorf("durable list runs: %w", err))
	}
	return runs, nil
}

func (r *Runtime) DeleteRuns(ctx context.Context, keys []string) (int, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	encoded, err := json.Marshal(keys)
	if err != nil {
		return 0, fmt.Errorf("durable delete runs: %w", err)
	}
	res, err := dbsqlite.Conn(ctx, r.db).ExecContext(ctx,
		`DELETE FROM durable_run WHERE key IN (SELECT value FROM json_each(?))`, string(encoded),
	)
	if err != nil {
		return 0, dbsqlite.Classify(fmt.Errorf("durable delete runs: %w", err))
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, dbsqlite.Classify(fmt.Errorf("durable delete runs: %w", err))
	}
	return int(deleted), nil
}
