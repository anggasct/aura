package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const HandoffReplyEventPrefix = "handoff-reply-"

func HandoffReplyEventID(at time.Time) string {
	return HandoffReplyEventPrefix + strconv.FormatInt(at.UnixNano(), 10)
}

type HandoffReply struct {
	Sequence  uint64
	Text      string
	CreatedAt time.Time
}

type HandoffReplyStore interface {
	List(ctx context.Context, sessionID string, afterSequence uint64, limit int) ([]HandoffReply, error)
}

type sqliteHandoffReplyStore struct {
	db *sql.DB
}

func NewHandoffReplyStore(db *sql.DB) HandoffReplyStore {
	return &sqliteHandoffReplyStore{db: db}
}

func (s *sqliteHandoffReplyStore) List(ctx context.Context, sessionID string, afterSequence uint64, limit int) ([]HandoffReply, error) {
	if limit < 1 {
		return nil, &Error{Code: ErrorCodeInvalidArgument, Detail: "list handoff replies: limit must be positive"}
	}
	after, err := sequenceToDB(afterSequence)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+selectRuntimeEventColumns+`
		 FROM runtime_event
		 WHERE session_id = ? AND sequence > ? AND kind = ? AND substr(id, 1, ?) = ?
		 ORDER BY sequence ASC
		 LIMIT ?`,
		sessionID, after, EventKindADK, len(HandoffReplyEventPrefix), HandoffReplyEventPrefix, limit,
	)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("list handoff replies: %w", err))
	}
	defer func() { _ = rows.Close() }()
	replies := make([]HandoffReply, 0, limit)
	for rows.Next() {
		event, err := scanRuntimeEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("list handoff replies: %w", err)
		}
		reply, err := handoffReplyOf(&event)
		if err != nil {
			return nil, err
		}
		replies = append(replies, reply)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list handoff replies: %w", err)
	}
	return replies, nil
}

func handoffReplyOf(event *RuntimeEvent) (HandoffReply, error) {
	decoded, err := RuntimeEventToADK(event)
	if err != nil {
		return HandoffReply{}, fmt.Errorf("list handoff replies: %w", err)
	}
	var text strings.Builder
	if decoded.Content != nil {
		for _, part := range decoded.Content.Parts {
			if part != nil && !part.Thought {
				text.WriteString(part.Text)
			}
		}
	}
	return HandoffReply{Sequence: event.Sequence, Text: text.String(), CreatedAt: event.CreatedAt}, nil
}
