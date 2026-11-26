package turnstore

import (
	"context"
	"fmt"

	"github.com/anggasct/aura/internal/platform/sqlite"
)

type RowCounts struct {
	Sessions   int
	Events     int
	DedupeKeys int
}

func SessionExists(ctx context.Context, q sqlite.Querier, id string) bool {
	if q == nil {
		return false
	}
	return rowExists(ctx, q, `SELECT 1 FROM session WHERE id = ?`, id)
}

func EventExists(ctx context.Context, q sqlite.Querier, id string) bool {
	if q == nil {
		return false
	}
	return rowExists(ctx, q, `SELECT 1 FROM runtime_event WHERE id = ?`, id)
}

func CountRows(ctx context.Context, q sqlite.Querier) (RowCounts, error) {
	if q == nil {
		return RowCounts{}, errNilArgument("querier")
	}
	var counts RowCounts
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM session`).Scan(&counts.Sessions); err != nil {
		return RowCounts{}, classify(fmt.Errorf("count sessions: %w", err))
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_event`).Scan(&counts.Events); err != nil {
		return RowCounts{}, classify(fmt.Errorf("count events: %w", err))
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM ingress_dedupe`).Scan(&counts.DedupeKeys); err != nil {
		return RowCounts{}, classify(fmt.Errorf("count dedupe keys: %w", err))
	}
	return counts, nil
}
