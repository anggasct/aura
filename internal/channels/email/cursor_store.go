package email

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

type SQLiteCursorStore struct {
	db *sql.DB
}

var _ CursorStore = (*SQLiteCursorStore)(nil)

func NewSQLiteCursorStore(db *sql.DB) (*SQLiteCursorStore, error) {
	if db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	return &SQLiteCursorStore{db: db}, nil
}

func (s *SQLiteCursorStore) Load(ctx context.Context, source string) (cursor Cursor, found bool, err error) {
	if s == nil || s.db == nil {
		return Cursor{}, false, fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	var validity, lastUID int64
	err = sqlite.Conn(ctx, s.db).QueryRowContext(ctx, `SELECT uid_validity, last_uid FROM email_cursor WHERE source = ?`, source).Scan(&validity, &lastUID)
	if errors.Is(err, sql.ErrNoRows) {
		return Cursor{}, false, nil
	}
	if err != nil {
		return Cursor{}, false, sqlite.Classify(fmt.Errorf("load email cursor: %w", err))
	}
	if validity < 0 || validity > math.MaxUint32 || lastUID < 0 || lastUID > math.MaxUint32 {
		return Cursor{}, false, fault.New(ErrorCodeInvalidArgument, "stored email cursor is out of range")
	}
	return Cursor{UIDValidity: uint32(validity), LastUID: uint32(lastUID)}, true, nil
}

func (s *SQLiteCursorStore) Save(ctx context.Context, source string, cursor Cursor, at time.Time) error {
	if s == nil || s.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if source == "" {
		return fault.New(ErrorCodeInvalidArgument, "source must not be empty")
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
	if _, err := sqlite.Conn(ctx, s.db).ExecContext(ctx, query, source, int64(cursor.UIDValidity), int64(cursor.LastUID), sqlite.FormatTime(at.UTC())); err != nil {
		return sqlite.Classify(fmt.Errorf("save email cursor: %w", err))
	}
	return nil
}
