package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/store"
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
	turns *turnstore.TurnStore
}

func NewHandoffReplyStore(db *sql.DB) HandoffReplyStore {
	return &sqliteHandoffReplyStore{turns: turnstore.NewTurnStore(db)}
}

func (s *sqliteHandoffReplyStore) List(ctx context.Context, sessionID string, afterSequence uint64, limit int) ([]HandoffReply, error) {
	if limit < 1 {
		return nil, fault.New(ErrorCodeInvalidArgument, "list handoff replies: limit must be positive")
	}
	events, err := s.turns.ListSessionEvents(ctx, sessionID, turn.EventFilter{
		AfterSequence: afterSequence,
		Kind:          turnstore.EventKindADK,
		IDPrefix:      HandoffReplyEventPrefix,
		Limit:         limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list handoff replies: %w", err)
	}
	replies := make([]HandoffReply, 0, len(events))
	for i := range events {
		reply, err := handoffReplyOf(&events[i])
		if err != nil {
			return nil, err
		}
		replies = append(replies, reply)
	}
	return replies, nil
}

func handoffReplyOf(event *turn.Event) (HandoffReply, error) {
	decoded, err := turnstore.RuntimeEventToADK(event)
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
