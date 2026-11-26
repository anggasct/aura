package turnstore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
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
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, query, args...)
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

func (s *TurnStore) CreateSession(ctx context.Context, sess *turn.Session) error {
	return createSession(ctx, sqlite.Conn(ctx, s.db), sess)
}

func (s *TurnStore) AllocateSequence(ctx context.Context, sessionID string) (uint64, error) {
	if sessionID == "" {
		return 0, fault.New(ErrorCodeInvalidArgument, "session id must not be empty")
	}
	q := sqlite.Conn(ctx, s.db)
	if !rowExists(ctx, q, `SELECT 1 FROM session WHERE id = ?`, sessionID) {
		return 0, fault.New(ErrorCodeSessionNotFound, "session does not exist")
	}
	sequence, err := assignNextSequence(ctx, q, sessionID)
	if err != nil {
		return 0, classify(fmt.Errorf("allocate sequence: %w", err))
	}
	return sequence, nil
}

func (s *TurnStore) AppendEvent(ctx context.Context, e *turn.Event) error {
	if e == nil {
		return errNilArgument("event")
	}
	return appendEvent(ctx, sqlite.Conn(ctx, s.db), e)
}

func (s *TurnStore) ClaimDedupe(ctx context.Context, claim *turn.DedupeClaim) (originalTurnID string, created bool, err error) {
	return claimDedupe(ctx, sqlite.Conn(ctx, s.db), claim)
}

func (s *TurnStore) TurnUsed(ctx context.Context, turnID string) (bool, error) {
	var used bool
	if err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM runtime_event WHERE turn_id = ?)`, turnID,
	).Scan(&used); err != nil {
		return false, classify(fmt.Errorf("read turn use %s: %w", turnID, err))
	}
	return used, nil
}

func (s *TurnStore) SessionOwner(ctx context.Context, sessionID string) (string, error) {
	var ownerID string
	if err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx,
		`SELECT owner_id FROM session WHERE id = ?`, sessionID,
	).Scan(&ownerID); err != nil {
		return "", classify(fmt.Errorf("read session %s owner: %w", sessionID, err))
	}
	return ownerID, nil
}
