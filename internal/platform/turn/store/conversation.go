package turnstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/platform/sqlite"
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
		return "", false, errorf(ErrorCodeInvalidArgument, "conversation key must not be empty")
	}
	err = sqlite.Conn(ctx, s.db).QueryRowContext(ctx,
		`SELECT session_id FROM conversation_session WHERE conversation_key = ?`, conversationKey,
	).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, classify(fmt.Errorf("resolve conversation session: %w", err))
	}
	return sessionID, true, nil
}

func (s *sqliteConversationSessionStore) Point(ctx context.Context, conversationKey, sessionID string) error {
	if conversationKey == "" || sessionID == "" {
		return errorf(ErrorCodeInvalidArgument, "conversation key and session id must not be empty")
	}
	_, err := sqlite.Conn(ctx, s.db).ExecContext(ctx, `
INSERT INTO conversation_session (conversation_key, session_id, updated_at) VALUES (?, ?, ?)
ON CONFLICT(conversation_key) DO UPDATE SET
    session_id = excluded.session_id,
    updated_at = excluded.updated_at`,
		conversationKey, sessionID, sqlite.FormatTime(s.now().UTC()),
	)
	if err != nil {
		if isConstraintForeignKey(err) {
			return errorf(ErrorCodeSessionNotFound, "pointed session does not exist")
		}
		return classify(fmt.Errorf("point conversation session: %w", err))
	}
	return nil
}

func (s *sqliteConversationSessionStore) Clear(ctx context.Context, conversationKey string) error {
	if conversationKey == "" {
		return errorf(ErrorCodeInvalidArgument, "conversation key must not be empty")
	}
	if _, err := sqlite.Conn(ctx, s.db).ExecContext(ctx, `DELETE FROM conversation_session WHERE conversation_key = ?`, conversationKey); err != nil {
		return classify(fmt.Errorf("clear conversation session: %w", err))
	}
	return nil
}
