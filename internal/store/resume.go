package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type ChannelResume struct {
	Source           string
	Instance         string
	GatewaySessionID string
	LastSequence     int64
	ConfigDigest     string
	UpdatedAt        time.Time
}

type ChannelResumeStore interface {
	Save(ctx context.Context, resume *ChannelResume) error
	Load(ctx context.Context, source, instance string) (ChannelResume, bool, error)
	Delete(ctx context.Context, source, instance string) error
}

type sqliteChannelResumeStore struct {
	db *sql.DB
}

func NewChannelResumeStore(db *sql.DB) ChannelResumeStore {
	return &sqliteChannelResumeStore{db: db}
}

func (s *sqliteChannelResumeStore) Save(ctx context.Context, resume *ChannelResume) error {
	if s.db == nil || resume == nil {
		return nil
	}
	if resume.Source == "" {
		return Errorf(ErrorCodeInvalidArgument, "source must not be empty")
	}
	if resume.Instance == "" {
		return Errorf(ErrorCodeInvalidArgument, "instance must not be empty")
	}
	if resume.GatewaySessionID == "" {
		return Errorf(ErrorCodeInvalidArgument, "gateway session id must not be empty")
	}
	if resume.LastSequence < 0 {
		return Errorf(ErrorCodeInvalidArgument, "last sequence must not be negative")
	}

	query := `
INSERT INTO channel_resume (
    source, instance, gateway_session_id, last_sequence, config_digest, updated_at
) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(source, instance) DO UPDATE SET
    gateway_session_id = excluded.gateway_session_id,
    last_sequence = excluded.last_sequence,
    config_digest = excluded.config_digest,
    updated_at = excluded.updated_at
`
	_, err := s.db.ExecContext(ctx, query,
		resume.Source,
		resume.Instance,
		resume.GatewaySessionID,
		resume.LastSequence,
		resume.ConfigDigest,
		formatTime(resume.UpdatedAt.UTC()),
	)
	if err != nil {
		return classifyBusy(fmt.Errorf("save channel resume: %w", err))
	}
	return nil
}

func (s *sqliteChannelResumeStore) Load(ctx context.Context, source, instance string) (ChannelResume, bool, error) {
	if s.db == nil {
		return ChannelResume{}, false, nil
	}
	query := `
SELECT source, instance, gateway_session_id, last_sequence, config_digest, updated_at
FROM channel_resume
WHERE source = ? AND instance = ?
`
	var resume ChannelResume
	var updatedAtRaw string
	err := s.db.QueryRowContext(ctx, query, source, instance).Scan(
		&resume.Source,
		&resume.Instance,
		&resume.GatewaySessionID,
		&resume.LastSequence,
		&resume.ConfigDigest,
		&updatedAtRaw,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ChannelResume{}, false, nil
	}
	if err != nil {
		return ChannelResume{}, false, classifyBusy(fmt.Errorf("load channel resume: %w", err))
	}
	updatedAt, err := parseTime(updatedAtRaw)
	if err != nil {
		return ChannelResume{}, false, Errorf(ErrorCodeChannelResumeInvalid, "channel resume updated_at is not a valid timestamp")
	}
	resume.UpdatedAt = updatedAt
	return resume, true, nil
}

func (s *sqliteChannelResumeStore) Delete(ctx context.Context, source, instance string) error {
	if s.db == nil {
		return nil
	}
	query := `DELETE FROM channel_resume WHERE source = ? AND instance = ?`
	_, err := s.db.ExecContext(ctx, query, source, instance)
	if err != nil {
		return classifyBusy(fmt.Errorf("delete channel resume: %w", err))
	}
	return nil
}
