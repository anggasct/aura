package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	HandoffNone    = "none"
	HandoffOpen    = "open"
	HandoffExpired = "expired"
)

const (
	HandoffChannelDiscord  = "discord"
	HandoffChannelTelegram = "telegram"
	HandoffChannelHTTP     = "http"
	HandoffChannelEmail    = "email"
)

type Handoff struct {
	SessionID      string
	Reason         string
	PrincipalID    string
	AgentID        string
	Account        string
	Channel        string
	ReplyChannelID string
	ReplyThreadID  string
	OpenedAt       time.Time
	ReleasedAt     time.Time
}

type HandoffStore interface {
	Open(ctx context.Context, handoff *Handoff) error
	Check(ctx context.Context, sessionID string, timeout time.Duration, now time.Time) (string, Handoff, error)
	Release(ctx context.Context, sessionID string, now time.Time) (Handoff, bool, error)
	Get(ctx context.Context, sessionID string) (Handoff, bool, error)
	ListOpen(ctx context.Context) ([]Handoff, error)
}

type sqliteHandoffStore struct {
	db *sql.DB
}

func NewHandoffStore(db *sql.DB) HandoffStore {
	return &sqliteHandoffStore{db: db}
}

const handoffColumns = `session_id, reason, principal_id, agent_id, account, channel, reply_channel_id, reply_thread_id, opened_at, COALESCE(released_at, '')`

func (s *sqliteHandoffStore) Open(ctx context.Context, handoff *Handoff) error {
	if handoff == nil {
		return errNilArgument("handoff")
	}
	if handoff.SessionID == "" || handoff.Reason == "" {
		return &Error{Code: ErrorCodeInvalidArgument, Detail: "open handoff: session and reason are required"}
	}
	switch handoff.Channel {
	case HandoffChannelDiscord, HandoffChannelTelegram, HandoffChannelHTTP, HandoffChannelEmail:
	default:
		return &Error{Code: ErrorCodeInvalidArgument, Detail: "open handoff: channel must be discord, telegram, http or email"}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO session_handoff (session_id, reason, principal_id, agent_id, account, channel, reply_channel_id, reply_thread_id, opened_at, released_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
		ON CONFLICT(session_id) DO UPDATE SET
			reason = excluded.reason, principal_id = excluded.principal_id, agent_id = excluded.agent_id,
			account = excluded.account, channel = excluded.channel, reply_channel_id = excluded.reply_channel_id,
			reply_thread_id = excluded.reply_thread_id, opened_at = excluded.opened_at, released_at = NULL`,
		handoff.SessionID, handoff.Reason, handoff.PrincipalID, handoff.AgentID, handoff.Account, handoff.Channel,
		handoff.ReplyChannelID, handoff.ReplyThreadID, formatTime(handoff.OpenedAt))
	if err != nil {
		if isConstraintCheck(err) {
			return codedError(ErrorCodeInvalidArgument, "open handoff: a column value violates its constraint", err)
		}
		return classifyBusy(fmt.Errorf("open handoff: %w", err))
	}
	return nil
}

func (s *sqliteHandoffStore) Get(ctx context.Context, sessionID string) (Handoff, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+handoffColumns+` FROM session_handoff WHERE session_id = ?`, sessionID)
	handoff, err := scanHandoff(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Handoff{}, false, nil
	}
	if err != nil {
		return Handoff{}, false, err
	}
	return handoff, true, nil
}

func (s *sqliteHandoffStore) Check(ctx context.Context, sessionID string, timeout time.Duration, now time.Time) (string, Handoff, error) {
	handoff, found, err := s.Get(ctx, sessionID)
	if err != nil || !found || !handoff.ReleasedAt.IsZero() {
		return HandoffNone, handoff, err
	}
	if now.Before(handoff.OpenedAt.Add(timeout)) {
		return HandoffOpen, handoff, nil
	}
	released, ok, err := s.Release(ctx, sessionID, now)
	if err != nil || !ok {
		return HandoffNone, handoff, err
	}
	return HandoffExpired, released, nil
}

func (s *sqliteHandoffStore) Release(ctx context.Context, sessionID string, now time.Time) (Handoff, bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE session_handoff SET released_at = ? WHERE session_id = ? AND released_at IS NULL`, formatTime(now), sessionID)
	if err != nil {
		return Handoff{}, false, classifyBusy(fmt.Errorf("release handoff: %w", err))
	}
	if changed, err := result.RowsAffected(); err != nil || changed == 0 {
		return Handoff{}, false, err
	}
	return s.Get(ctx, sessionID)
}

func (s *sqliteHandoffStore) ListOpen(ctx context.Context) ([]Handoff, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+handoffColumns+` FROM session_handoff WHERE released_at IS NULL ORDER BY opened_at, session_id`)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("list handoffs: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var handoffs []Handoff
	for rows.Next() {
		handoff, err := scanHandoff(rows)
		if err != nil {
			return nil, err
		}
		handoffs = append(handoffs, handoff)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list handoffs: %w", err)
	}
	return handoffs, nil
}

func scanHandoff(row rowScanner) (Handoff, error) {
	var handoff Handoff
	var openedAt, releasedAt string
	if err := row.Scan(&handoff.SessionID, &handoff.Reason, &handoff.PrincipalID, &handoff.AgentID, &handoff.Account, &handoff.Channel, &handoff.ReplyChannelID, &handoff.ReplyThreadID, &openedAt, &releasedAt); err != nil {
		return Handoff{}, err
	}
	opened, err := parseTime(openedAt)
	if err != nil {
		return Handoff{}, fmt.Errorf("parse handoff opened_at: %w", err)
	}
	handoff.OpenedAt = opened
	if releasedAt != "" {
		released, err := parseTime(releasedAt)
		if err != nil {
			return Handoff{}, fmt.Errorf("parse handoff released_at: %w", err)
		}
		handoff.ReleasedAt = released
	}
	return handoff, nil
}
