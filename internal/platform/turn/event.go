package turn

import (
	"encoding/json"
	"time"
)

type Session struct {
	ID        string
	OwnerID   string
	AgentID   string
	CreatedAt time.Time
	UpdatedAt time.Time
	Metadata  json.RawMessage
}

type Event struct {
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

type DedupeClaim struct {
	Source     string
	ExternalID string
	TurnID     string
	Now        time.Time
	ExpiresAt  time.Time
}
