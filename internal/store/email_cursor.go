package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

type EmailCursor struct {
	UIDValidity uint32
	LastUID     uint32
}

type EmailCursorStore interface {
	Load(ctx context.Context, source string) (cursor EmailCursor, found bool, err error)
	Save(ctx context.Context, source string, cursor EmailCursor, at time.Time) error
}

type sqliteEmailCursorStore struct {
	db *sql.DB
}

func NewEmailCursorStore(db *sql.DB) EmailCursorStore {
	return &sqliteEmailCursorStore{db: db}
}

func (s *sqliteEmailCursorStore) Load(ctx context.Context, source string) (cursor EmailCursor, found bool, err error) {
	if s.db == nil {
		return EmailCursor{}, false, nil
	}
	var validity, lastUID int64
	err = s.db.QueryRowContext(ctx, `SELECT uid_validity, last_uid FROM email_cursor WHERE source = ?`, source).Scan(&validity, &lastUID)
	if errors.Is(err, sql.ErrNoRows) {
		return EmailCursor{}, false, nil
	}
	if err != nil {
		return EmailCursor{}, false, classifyBusy(fmt.Errorf("load email cursor: %w", err))
	}
	if validity < 0 || validity > math.MaxUint32 || lastUID < 0 || lastUID > math.MaxUint32 {
		return EmailCursor{}, false, Errorf(ErrorCodeInvalidArgument, "stored email cursor is out of range")
	}
	return EmailCursor{UIDValidity: uint32(validity), LastUID: uint32(lastUID)}, true, nil
}

func (s *sqliteEmailCursorStore) Save(ctx context.Context, source string, cursor EmailCursor, at time.Time) error {
	if s.db == nil {
		return nil
	}
	if source == "" {
		return Errorf(ErrorCodeInvalidArgument, "source must not be empty")
	}
	query := `
INSERT INTO email_cursor (source, uid_validity, last_uid, updated_at) VALUES (?, ?, ?, ?)
ON CONFLICT(source) DO UPDATE SET
    last_uid = CASE
        WHEN email_cursor.uid_validity = excluded.uid_validity THEN MAX(email_cursor.last_uid, excluded.last_uid)
        ELSE excluded.last_uid
    END,
    uid_validity = excluded.uid_validity,
    updated_at = excluded.updated_at
`
	if _, err := s.db.ExecContext(ctx, query, source, int64(cursor.UIDValidity), int64(cursor.LastUID), formatTime(at.UTC())); err != nil {
		return classifyBusy(fmt.Errorf("save email cursor: %w", err))
	}
	return nil
}
