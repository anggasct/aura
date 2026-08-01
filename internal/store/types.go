package store

import (
	"context"
	"encoding/json"
	"time"
)

type Session struct {
	ID        string
	OwnerID   string
	CreatedAt time.Time
	UpdatedAt time.Time
	Metadata  json.RawMessage
}

type RuntimeEvent struct {
	ID            string
	SessionID     string
	Sequence      uint64
	TurnID        string
	InvocationID  string
	Branch        string
	Author        string
	Kind          string
	SchemaVersion uint16
	Payload       json.RawMessage
	ProviderUsage json.RawMessage
	CreatedAt     time.Time
}

// Pointer parameters are required; a nil argument returns ErrorCodeInvalidArgument.
type SessionService interface {
	Create(ctx context.Context, sess *Session) error
	Get(ctx context.Context, sessionID string) (Session, error)
	ListEvents(ctx context.Context, sessionID string, afterSequence uint64, limit int) ([]RuntimeEvent, error)
}

type EventStore interface {
	Append(ctx context.Context, e *RuntimeEvent) error
	AppendBatch(ctx context.Context, events []RuntimeEvent) error
	LastSequence(ctx context.Context, sessionID string) (uint64, error)
}
