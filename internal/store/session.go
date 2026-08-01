package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

type sqliteSessionService struct {
	db *sql.DB
}

func NewSessionService(db *sql.DB) SessionService {
	return &sqliteSessionService{db: db}
}

func (s *sqliteSessionService) Create(ctx context.Context, sess Session) error {
	metadata := sess.Metadata
	if metadata == nil {
		metadata = json.RawMessage(`{}`)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO session (id, owner_id, metadata_json, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		sess.ID, sess.OwnerID, string(metadata), formatTime(sess.CreatedAt), formatTime(sess.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("create session %s: %w", sess.ID, err)
	}
	return nil
}

func (s *sqliteSessionService) Get(ctx context.Context, id string) (Session, error) {
	var sess Session
	var metadata, createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, owner_id, metadata_json, created_at, updated_at FROM session WHERE id = ?`, id,
	).Scan(&sess.ID, &sess.OwnerID, &metadata, &createdAt, &updatedAt)
	if err != nil {
		return Session{}, fmt.Errorf("get session %s: %w", id, err)
	}
	sess.Metadata = json.RawMessage(metadata)
	if sess.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return Session{}, fmt.Errorf("parse created_at for session %s: %w", id, err)
	}
	if sess.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return Session{}, fmt.Errorf("parse updated_at for session %s: %w", id, err)
	}
	return sess, nil
}

func (s *sqliteSessionService) ListEvents(ctx context.Context, sessionID string, after uint64, limit int) ([]RuntimeEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, sequence, turn_id, invocation_id, branch, author, kind,
		        schema_version, payload_json, provider_usage_json, created_at
		 FROM runtime_event
		 WHERE session_id = ? AND sequence > ?
		 ORDER BY sequence ASC
		 LIMIT ?`,
		sessionID, int64(after), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list events for session %s: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()

	var events []RuntimeEvent
	for rows.Next() {
		var e RuntimeEvent
		var sequence, schemaVersion int64
		var payload, createdAt string
		var providerUsage sql.NullString
		if err := rows.Scan(&e.ID, &e.SessionID, &sequence, &e.TurnID, &e.InvocationID, &e.Branch, &e.Author, &e.Kind,
			&schemaVersion, &payload, &providerUsage, &createdAt); err != nil {
			return nil, fmt.Errorf("scan event for session %s: %w", sessionID, err)
		}
		e.Sequence = uint64(sequence)
		e.SchemaVersion = uint16(schemaVersion)
		e.Payload = json.RawMessage(payload)
		if providerUsage.Valid {
			e.ProviderUsage = json.RawMessage(providerUsage.String)
		}
		if e.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
			return nil, fmt.Errorf("parse created_at for event %s: %w", e.ID, err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list events for session %s: %w", sessionID, err)
	}
	return events, nil
}
