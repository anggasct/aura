package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type TelegramCursorStore interface {
	Load(ctx context.Context, source string) (nextUpdateID int64, found bool, err error)
	Advance(ctx context.Context, source string, nextUpdateID int64, at time.Time) error
}

type sqliteTelegramCursorStore struct {
	db *sql.DB
}

func NewTelegramCursorStore(db *sql.DB) TelegramCursorStore {
	return &sqliteTelegramCursorStore{db: db}
}

func (s *sqliteTelegramCursorStore) Load(ctx context.Context, source string) (nextUpdateID int64, found bool, err error) {
	if s.db == nil {
		return 0, false, nil
	}
	err = s.db.QueryRowContext(ctx, `SELECT next_update_id FROM telegram_cursor WHERE source = ?`, source).Scan(&nextUpdateID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, classifyBusy(fmt.Errorf("load telegram cursor: %w", err))
	}
	return nextUpdateID, true, nil
}

func (s *sqliteTelegramCursorStore) Advance(ctx context.Context, source string, nextUpdateID int64, at time.Time) error {
	if s.db == nil {
		return nil
	}
	if source == "" {
		return Errorf(ErrorCodeInvalidArgument, "source must not be empty")
	}
	if nextUpdateID < 0 {
		return Errorf(ErrorCodeInvalidArgument, "next update id must not be negative")
	}
	query := `
INSERT INTO telegram_cursor (source, next_update_id, updated_at) VALUES (?, ?, ?)
ON CONFLICT(source) DO UPDATE SET
    next_update_id = MAX(telegram_cursor.next_update_id, excluded.next_update_id),
    updated_at = excluded.updated_at
`
	if _, err := s.db.ExecContext(ctx, query, source, nextUpdateID, formatTime(at.UTC())); err != nil {
		return classifyBusy(fmt.Errorf("advance telegram cursor: %w", err))
	}
	return nil
}
