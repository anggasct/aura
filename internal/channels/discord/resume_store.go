package discord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

type SQLiteResumeStore struct {
	db       *sql.DB
	instance string
}

var _ ResumeStore = (*SQLiteResumeStore)(nil)

func NewSQLiteResumeStore(db *sql.DB, instance string) (*SQLiteResumeStore, error) {
	if db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if instance == "" {
		return nil, fault.New(ErrorCodeInvalidArgument, "instance must not be empty")
	}
	return &SQLiteResumeStore{db: db, instance: instance}, nil
}

func (s *SQLiteResumeStore) Save(ctx context.Context, cursor *ResumeCursor) error {
	if s == nil || s.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "resume store must not be nil")
	}
	if cursor == nil {
		return fault.New(ErrorCodeInvalidArgument, "resume cursor must not be nil")
	}
	if cursor.SessionID == "" {
		return fault.New(ErrorCodeInvalidArgument, "gateway session id must not be empty")
	}
	if cursor.Sequence < 0 {
		return fault.New(ErrorCodeInvalidArgument, "last sequence must not be negative")
	}

	const query = `
INSERT INTO channel_resume (
    source, instance, gateway_session_id, last_sequence, config_digest, updated_at
) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(source, instance) DO UPDATE SET
    gateway_session_id = excluded.gateway_session_id,
    last_sequence = excluded.last_sequence,
    config_digest = excluded.config_digest,
    updated_at = excluded.updated_at
`
	_, err := sqlite.Conn(ctx, s.db).ExecContext(ctx, query,
		principal.ChannelDiscord,
		s.instance,
		cursor.SessionID,
		cursor.Sequence,
		cursor.ConfigDigest,
		sqlite.FormatTime(cursor.UpdatedAt.UTC()),
	)
	if err != nil {
		return sqlite.Classify(fmt.Errorf("save channel resume: %w", err))
	}
	return nil
}

func (s *SQLiteResumeStore) Load(ctx context.Context) (ResumeCursor, bool, error) {
	if s == nil || s.db == nil {
		return ResumeCursor{}, false, fault.New(ErrorCodeInvalidArgument, "resume store must not be nil")
	}
	const query = `
SELECT gateway_session_id, last_sequence, config_digest, updated_at
FROM channel_resume
WHERE source = ? AND instance = ?
`
	var cursor ResumeCursor
	var updatedAtRaw string
	err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx, query, principal.ChannelDiscord, s.instance).Scan(
		&cursor.SessionID,
		&cursor.Sequence,
		&cursor.ConfigDigest,
		&updatedAtRaw,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ResumeCursor{}, false, nil
	}
	if err != nil {
		return ResumeCursor{}, false, sqlite.Classify(fmt.Errorf("load channel resume: %w", err))
	}
	updatedAt, err := sqlite.ParseTime(updatedAtRaw)
	if err != nil {
		return ResumeCursor{}, false, fault.New(ErrorCodeResumeInvalid, "channel resume updated_at is not a valid timestamp")
	}
	cursor.UpdatedAt = updatedAt
	return cursor, true, nil
}

func (s *SQLiteResumeStore) Delete(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "resume store must not be nil")
	}
	const query = `DELETE FROM channel_resume WHERE source = ? AND instance = ?`
	_, err := sqlite.Conn(ctx, s.db).ExecContext(ctx, query, principal.ChannelDiscord, s.instance)
	if err != nil {
		return sqlite.Classify(fmt.Errorf("delete channel resume: %w", err))
	}
	return nil
}
