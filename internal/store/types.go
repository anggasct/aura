package store

import (
	"context"

	"github.com/anggasct/aura/internal/platform/turn"
)

type SessionService interface {
	Create(ctx context.Context, sess *turn.Session) error
	Get(ctx context.Context, sessionID string) (turn.Session, error)
	ListEvents(ctx context.Context, sessionID string, afterSequence uint64, limit int) ([]turn.Event, error)
}

type rowScanner interface {
	Scan(dest ...any) error
}

type EventStore interface {
	Append(ctx context.Context, e *turn.Event) error
	AppendBatch(ctx context.Context, events []turn.Event) error
	LastSequence(ctx context.Context, sessionID string) (uint64, error)
	AppendSequenced(ctx context.Context, sessionID string, e *turn.Event) (uint64, error)
	UpsertEvent(ctx context.Context, e *turn.Event) (uint64, bool, error)
}
