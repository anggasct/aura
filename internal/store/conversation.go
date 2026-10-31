package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type ConversationSessionStore interface {
	Resolve(ctx context.Context, conversationKey string) (sessionID string, found bool, err error)
	Point(ctx context.Context, conversationKey, sessionID string) error
	Clear(ctx context.Context, conversationKey string) error
}

type sqliteConversationSessionStore struct {
	db  *sql.DB
	now func() time.Time
}

func NewConversationSessionStore(db *sql.DB) ConversationSessionStore {
	return &sqliteConversationSessionStore{db: db, now: time.Now}
}

func (s *sqliteConversationSessionStore) Resolve(ctx context.Context, conversationKey string) (sessionID string, found bool, err error) {
	if conversationKey == "" {
		return "", false, Errorf(ErrorCodeInvalidArgument, "conversation key must not be empty")
	}
	err = s.db.QueryRowContext(ctx,
		`SELECT session_id FROM conversation_session WHERE conversation_key = ?`, conversationKey,
	).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, classifyBusy(fmt.Errorf("resolve conversation session: %w", err))
	}
	return sessionID, true, nil
}

func (s *sqliteConversationSessionStore) Point(ctx context.Context, conversationKey, sessionID string) error {
	if conversationKey == "" || sessionID == "" {
		return Errorf(ErrorCodeInvalidArgument, "conversation key and session id must not be empty")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO conversation_session (conversation_key, session_id, updated_at) VALUES (?, ?, ?)
ON CONFLICT(conversation_key) DO UPDATE SET
    session_id = excluded.session_id,
    updated_at = excluded.updated_at`,
		conversationKey, sessionID, formatTime(s.now().UTC()),
	)
	if err != nil {
		if isConstraintForeignKey(err) {
			return Errorf(ErrorCodeSessionNotFound, "pointed session does not exist")
		}
		return classifyBusy(fmt.Errorf("point conversation session: %w", err))
	}
	return nil
}

func (s *sqliteConversationSessionStore) Clear(ctx context.Context, conversationKey string) error {
	if conversationKey == "" {
		return Errorf(ErrorCodeInvalidArgument, "conversation key must not be empty")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM conversation_session WHERE conversation_key = ?`, conversationKey); err != nil {
		return classifyBusy(fmt.Errorf("clear conversation session: %w", err))
	}
	return nil
}
