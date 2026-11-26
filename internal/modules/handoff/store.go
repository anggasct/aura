package handoff

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const (
	StateNone    = "none"
	StateOpen    = "open"
	StateExpired = "expired"
)

const (
	ChannelDiscord  = "discord"
	ChannelTelegram = "telegram"
	ChannelHTTP     = "http"
	ChannelEmail    = "email"
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

type Store interface {
	Open(ctx context.Context, handoff *Handoff) error
	Check(ctx context.Context, sessionID string, timeout time.Duration, now time.Time) (string, Handoff, error)
	Release(ctx context.Context, sessionID string, now time.Time) (Handoff, bool, error)
	Get(ctx context.Context, sessionID string) (Handoff, bool, error)
	ListOpen(ctx context.Context) ([]Handoff, error)
}

type rowScanner interface {
	Scan(dest ...any) error
}

type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(db *sql.DB) (*SQLiteStore, error) {
	if db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "handoff database must not be nil")
	}
	return &SQLiteStore{db: db}, nil
}

const handoffColumns = `session_id, reason, principal_id, agent_id, account, channel, reply_channel_id, reply_thread_id, opened_at, COALESCE(released_at, '')`

func (s *SQLiteStore) Open(ctx context.Context, handoff *Handoff) error {
	if handoff == nil {
		return fault.New(ErrorCodeInvalidArgument, "handoff must not be nil")
	}
	if handoff.SessionID == "" || handoff.Reason == "" {
		return fault.New(ErrorCodeInvalidArgument, "open handoff: session and reason are required")
	}
	switch handoff.Channel {
	case ChannelDiscord, ChannelTelegram, ChannelHTTP, ChannelEmail:
	default:
		return fault.New(ErrorCodeInvalidArgument, "open handoff: channel must be discord, telegram, http or email")
	}
	_, err := sqlite.Conn(ctx, s.db).ExecContext(ctx, `
		INSERT INTO session_handoff (session_id, reason, principal_id, agent_id, account, channel, reply_channel_id, reply_thread_id, opened_at, released_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
		ON CONFLICT(session_id) DO UPDATE SET
			reason = excluded.reason, principal_id = excluded.principal_id, agent_id = excluded.agent_id,
			account = excluded.account, channel = excluded.channel, reply_channel_id = excluded.reply_channel_id,
			reply_thread_id = excluded.reply_thread_id, opened_at = excluded.opened_at, released_at = NULL`,
		handoff.SessionID, handoff.Reason, handoff.PrincipalID, handoff.AgentID, handoff.Account, handoff.Channel,
		handoff.ReplyChannelID, handoff.ReplyThreadID, sqlite.FormatTime(handoff.OpenedAt))
	if err != nil {
		classified := sqlite.Classify(err)
		if fault.HasCode(classified, sqlite.ErrorCodeConstraintCheck) {
			return fault.Wrap(ErrorCodeInvalidArgument, "open handoff: a column value violates its constraint", err)
		}
		return fmt.Errorf("open handoff: %w", classified)
	}
	return nil
}

func (s *SQLiteStore) Get(ctx context.Context, sessionID string) (Handoff, bool, error) {
	row := sqlite.Conn(ctx, s.db).QueryRowContext(ctx, `SELECT `+handoffColumns+` FROM session_handoff WHERE session_id = ?`, sessionID)
	handoff, err := scanHandoff(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Handoff{}, false, nil
	}
	if err != nil {
		return Handoff{}, false, sqlite.Classify(err)
	}
	return handoff, true, nil
}

func (s *SQLiteStore) Check(ctx context.Context, sessionID string, timeout time.Duration, now time.Time) (string, Handoff, error) {
	handoff, found, err := s.Get(ctx, sessionID)
	if err != nil || !found || !handoff.ReleasedAt.IsZero() {
		return StateNone, handoff, err
	}
	if now.Before(handoff.OpenedAt.Add(timeout)) {
		return StateOpen, handoff, nil
	}
	released, ok, err := s.Release(ctx, sessionID, now)
	if err != nil || !ok {
		return StateNone, handoff, err
	}
	return StateExpired, released, nil
}

func (s *SQLiteStore) Release(ctx context.Context, sessionID string, now time.Time) (Handoff, bool, error) {
	result, err := sqlite.Conn(ctx, s.db).ExecContext(ctx, `UPDATE session_handoff SET released_at = ? WHERE session_id = ? AND released_at IS NULL`, sqlite.FormatTime(now), sessionID)
	if err != nil {
		return Handoff{}, false, fmt.Errorf("release handoff: %w", sqlite.Classify(err))
	}
	if changed, err := result.RowsAffected(); err != nil || changed == 0 {
		return Handoff{}, false, err
	}
	return s.Get(ctx, sessionID)
}

func (s *SQLiteStore) ListOpen(ctx context.Context) ([]Handoff, error) {
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, `SELECT `+handoffColumns+` FROM session_handoff WHERE released_at IS NULL ORDER BY opened_at, session_id`)
	if err != nil {
		return nil, fmt.Errorf("list handoffs: %w", sqlite.Classify(err))
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
		return nil, fmt.Errorf("list handoffs: %w", sqlite.Classify(err))
	}
	return handoffs, nil
}

func scanHandoff(row rowScanner) (Handoff, error) {
	var handoff Handoff
	var openedAt, releasedAt string
	if err := row.Scan(&handoff.SessionID, &handoff.Reason, &handoff.PrincipalID, &handoff.AgentID, &handoff.Account, &handoff.Channel, &handoff.ReplyChannelID, &handoff.ReplyThreadID, &openedAt, &releasedAt); err != nil {
		return Handoff{}, err
	}
	opened, err := sqlite.ParseTime(openedAt)
	if err != nil {
		return Handoff{}, fmt.Errorf("parse handoff opened_at: %w", err)
	}
	handoff.OpenedAt = opened
	if releasedAt != "" {
		released, err := sqlite.ParseTime(releasedAt)
		if err != nil {
			return Handoff{}, fmt.Errorf("parse handoff released_at: %w", err)
		}
		handoff.ReleasedAt = released
	}
	return handoff, nil
}
