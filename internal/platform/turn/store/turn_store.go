package turnstore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/turn"
)

type TurnStore struct {
	db *sql.DB
}

func NewTurnStore(db *sql.DB) *TurnStore {
	return &TurnStore{db: db}
}

func (s *TurnStore) ListSessionEvents(ctx context.Context, sessionID string, filter turn.EventFilter) ([]turn.Event, error) {
	if filter.Limit < 0 {
		return nil, fault.New(ErrorCodeInvalidArgument, "limit must not be negative")
	}
	after, err := sequenceToDB(filter.AfterSequence)
	if err != nil {
		return nil, err
	}
	query := `SELECT ` + selectRuntimeEventColumns + ` FROM runtime_event WHERE session_id = ? AND sequence > ?`
	args := []any{sessionID, after}
	if filter.Kind != "" {
		query += ` AND kind = ?`
		args = append(args, filter.Kind)
	}
	if filter.IDPrefix != "" {
		query += ` AND substr(id, 1, ?) = ?`
		args = append(args, len(filter.IDPrefix), filter.IDPrefix)
	}
	query += ` ORDER BY sequence ASC`
	if filter.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, filter.Limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, classify(fmt.Errorf("list events for session %s: %w", sessionID, err))
	}
	defer func() { _ = rows.Close() }()

	var events []turn.Event
	for rows.Next() {
		e, err := scanRuntimeEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("list events for session %s: %w", sessionID, err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(fmt.Errorf("list events for session %s: %w", sessionID, err))
	}
	return events, nil
}
