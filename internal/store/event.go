package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"modernc.org/sqlite"
)

// SQLITE_CONSTRAINT_UNIQUE, see https://www.sqlite.org/rescode.html#constraint_unique.
const sqliteConstraintUnique = 2067

const insertRuntimeEventSQL = `
INSERT INTO runtime_event (
    id, session_id, sequence, turn_id, invocation_id, branch, author, kind,
    schema_version, payload_json, provider_usage_json, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO NOTHING`

type sqliteEventStore struct {
	db *sql.DB
}

func NewEventStore(db *sql.DB) EventStore {
	return &sqliteEventStore{db: db}
}

func (s *sqliteEventStore) Append(ctx context.Context, e RuntimeEvent) error {
	return appendEvent(ctx, s.db, e)
}

func (s *sqliteEventStore) AppendBatch(ctx context.Context, events []RuntimeEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin append batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, e := range events {
		if err := appendEvent(ctx, tx, e); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *sqliteEventStore) LastSequence(ctx context.Context, sessionID string) (uint64, error) {
	var seq sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(sequence) FROM runtime_event WHERE session_id = ?`, sessionID,
	).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("last sequence for session %s: %w", sessionID, err)
	}
	if !seq.Valid {
		return 0, nil
	}
	return uint64(seq.Int64), nil
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func appendEvent(ctx context.Context, x execer, e RuntimeEvent) error {
	_, err := x.ExecContext(ctx, insertRuntimeEventSQL,
		e.ID, e.SessionID, int64(e.Sequence), e.TurnID, e.InvocationID, e.Branch, e.Author, e.Kind,
		int(e.SchemaVersion), string(e.Payload), nullableJSON(e.ProviderUsage), formatTime(e.CreatedAt),
	)
	if err != nil {
		if isSequenceConflict(err) {
			return &Error{
				Code:   ErrorCodeEventSequenceConflict,
				Detail: fmt.Sprintf("session %s sequence %d already used by a different event", e.SessionID, e.Sequence),
			}
		}
		return fmt.Errorf("append event %s: %w", e.ID, err)
	}
	return nil
}

func isSequenceConflict(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == sqliteConstraintUnique
}

func nullableJSON(raw json.RawMessage) any {
	if raw == nil {
		return nil
	}
	return string(raw)
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

const selectRuntimeEventColumns = `id, session_id, sequence, turn_id, invocation_id, branch, author, kind,
	                                schema_version, payload_json, provider_usage_json, created_at`

func scanRuntimeEvent(rows *sql.Rows) (RuntimeEvent, error) {
	var e RuntimeEvent
	var sequence, schemaVersion int64
	var payload, createdAt string
	var providerUsage sql.NullString
	if err := rows.Scan(&e.ID, &e.SessionID, &sequence, &e.TurnID, &e.InvocationID, &e.Branch, &e.Author, &e.Kind,
		&schemaVersion, &payload, &providerUsage, &createdAt); err != nil {
		return RuntimeEvent{}, fmt.Errorf("scan runtime event: %w", err)
	}
	e.Sequence = uint64(sequence)
	e.SchemaVersion = uint16(schemaVersion)
	e.Payload = json.RawMessage(payload)
	if providerUsage.Valid {
		e.ProviderUsage = json.RawMessage(providerUsage.String)
	}
	var err error
	if e.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return RuntimeEvent{}, fmt.Errorf("parse created_at for event %s: %w", e.ID, err)
	}
	return e, nil
}
