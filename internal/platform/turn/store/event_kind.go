package turnstore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/anggasct/aura/internal/platform/turn"
)

type EventKindReader interface {
	LatestByKind(ctx context.Context, sessionID, kind string, limit int) ([]turn.Event, error)
}

func NewEventKindReader(db *sql.DB) EventKindReader {
	return &sqliteEventStore{db: db}
}

func (s *sqliteEventStore) LatestByKind(ctx context.Context, sessionID, kind string, limit int) ([]turn.Event, error) {
	if limit <= 0 {
		return nil, errorf(ErrorCodeInvalidArgument, "limit must be positive")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+selectRuntimeEventColumns+`
		 FROM runtime_event
		 WHERE session_id = ? AND kind = ?
		 ORDER BY sequence DESC
		 LIMIT ?`,
		sessionID, kind, limit,
	)
	if err != nil {
		return nil, classify(fmt.Errorf("list %s events for session %s: %w", kind, sessionID, err))
	}
	defer func() { _ = rows.Close() }()
	var events []turn.Event
	for rows.Next() {
		event, err := scanRuntimeEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("list %s events for session %s: %w", kind, sessionID, err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(fmt.Errorf("list %s events for session %s: %w", kind, sessionID, err))
	}
	return events, nil
}
