package webhook

import (
	"context"
	"time"
)

const StatusPathPrefix = "/webhook/executions/"

type AcceptedEvent struct {
	KeyID      string
	Nonce      string
	BodyDigest string
	Body       []byte
	Envelope   Envelope
}

type ExecutionRef struct {
	ExecutionID string
	TurnID      string
}

type ExecutionStatus struct {
	ExecutionID   string
	TurnID        string
	State         string
	ResultEventID string
	ErrorCode     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ExpiresAt     time.Time
}

type Dispatcher interface {
	Dispatch(ctx context.Context, event *AcceptedEvent) (ExecutionRef, error)
}

type StatusStore interface {
	ExecutionStatus(ctx context.Context, id string) (ExecutionStatus, error)
}
