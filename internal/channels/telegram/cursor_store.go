package telegram

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

type SQLiteCursorStore struct {
	db *sql.DB
}

func NewSQLiteCursorStore(db *sql.DB) (*SQLiteCursorStore, error) {
	if db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	return &SQLiteCursorStore{db: db}, nil
}

func (s *SQLiteCursorStore) Load(ctx context.Context, source string) (nextUpdateID int64, found bool, err error) {
	if s == nil || s.db == nil {
		return 0, false, fault.New(ErrorCodeInvalidArgument, "cursor store must not be nil")
	}
	err = sqlite.Conn(ctx, s.db).QueryRowContext(ctx, `SELECT next_update_id FROM telegram_cursor WHERE source = ?`, source).Scan(&nextUpdateID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, sqlite.Classify(fmt.Errorf("load telegram cursor: %w", err))
	}
	return nextUpdateID, true, nil
}

func (s *SQLiteCursorStore) Advance(ctx context.Context, source string, nextUpdateID int64, at time.Time) error {
	if s == nil || s.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "cursor store must not be nil")
	}
	if source == "" {
		return fault.New(ErrorCodeInvalidArgument, "source must not be empty")
	}
	if nextUpdateID < 0 {
		return fault.New(ErrorCodeInvalidArgument, "next update id must not be negative")
	}
	query := `
INSERT INTO telegram_cursor (source, next_update_id, updated_at) VALUES (?, ?, ?)
ON CONFLICT(source) DO UPDATE SET
    next_update_id = MAX(telegram_cursor.next_update_id, excluded.next_update_id),
    updated_at = excluded.updated_at
`
	if _, err := sqlite.Conn(ctx, s.db).ExecContext(ctx, query, source, nextUpdateID, sqlite.FormatTime(at.UTC())); err != nil {
		return sqlite.Classify(fmt.Errorf("advance telegram cursor: %w", err))
	}
	return nil
}
