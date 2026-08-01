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

type SessionService interface {
	Create(context.Context, Session) error
	Get(context.Context, string) (Session, error)
	ListEvents(context.Context, string, uint64, int) ([]RuntimeEvent, error)
}

type EventStore interface {
	Append(context.Context, RuntimeEvent) error
	AppendBatch(context.Context, []RuntimeEvent) error
	LastSequence(context.Context, string) (uint64, error)
}
