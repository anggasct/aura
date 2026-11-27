package handoff

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

const ReplyEventPrefix = "handoff-reply-"

func ReplyEventID(at time.Time) string {
	return ReplyEventPrefix + strconv.FormatInt(at.UnixNano(), 10)
}

type Reply struct {
	Sequence  uint64
	Text      string
	CreatedAt time.Time
}

type ReplyStore interface {
	List(ctx context.Context, sessionID string, afterSequence uint64, limit int) ([]Reply, error)
}

type EventLister interface {
	ListSessionEvents(ctx context.Context, sessionID string, filter turn.EventFilter) ([]turn.Event, error)
}

type SQLiteReplyStore struct {
	events EventLister
}

func NewReplyStore(events EventLister) (*SQLiteReplyStore, error) {
	if events == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "handoff reply event lister must not be nil")
	}
	return &SQLiteReplyStore{events: events}, nil
}

func (s *SQLiteReplyStore) List(ctx context.Context, sessionID string, afterSequence uint64, limit int) ([]Reply, error) {
	if limit < 1 {
		return nil, fault.New(ErrorCodeInvalidArgument, "list handoff replies: limit must be positive")
	}
	events, err := s.events.ListSessionEvents(ctx, sessionID, turn.EventFilter{
		AfterSequence: afterSequence,
		Kind:          turnstore.EventKindADK,
		IDPrefix:      ReplyEventPrefix,
		Limit:         limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list handoff replies: %w", err)
	}
	replies := make([]Reply, 0, len(events))
	for i := range events {
		reply, err := replyOf(&events[i])
		if err != nil {
			return nil, err
		}
		replies = append(replies, reply)
	}
	return replies, nil
}

func replyOf(event *turn.Event) (Reply, error) {
	decoded, err := turnstore.RuntimeEventToADK(event)
	if err != nil {
		return Reply{}, fmt.Errorf("list handoff replies: %w", err)
	}
	var text strings.Builder
	if decoded.Content != nil {
		for _, part := range decoded.Content.Parts {
			if part != nil && !part.Thought {
				text.WriteString(part.Text)
			}
		}
	}
	return Reply{Sequence: event.Sequence, Text: text.String(), CreatedAt: event.CreatedAt}, nil
}
