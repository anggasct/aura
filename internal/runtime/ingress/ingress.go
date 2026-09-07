package runtimeingress

import (
	"context"
	"encoding/json"
	"time"
)

type InputPart struct {
	Text string
}

type IngressEnvelope struct {
	Source         string
	ExternalID     string
	PrincipalID    string
	ConversationID string
	ReplyContext   json.RawMessage
	Parts          []InputPart
	ReceivedAt     time.Time
	TraceParent    string
}

type TurnRef struct {
	TurnID    string
	SessionID string
	Replayed  bool
}

type IngressSink interface {
	Accept(ctx context.Context, env *IngressEnvelope) (TurnRef, error)
}
