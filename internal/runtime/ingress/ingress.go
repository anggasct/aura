package runtimeingress

import (
	"context"
	"encoding/json"
	"time"
)

// InputPart is one normalized piece of a turn's input. Channel adapters map
// their wire format onto these parts before submission.
type InputPart struct {
	Text string
}

// IngressEnvelope is one normalized inbound message a channel adapter hands to
// the runtime. The adapter owns wire authentication and normalization; the
// runtime owns identity validation, dedupe, and the turn. Source and
// ExternalID form the dedupe key, so a provider redelivery maps back to the
// original turn.
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

// TurnRef identifies an accepted turn. A duplicate delivery carries the
// original turn's reference with Replayed set, so an adapter can correlate
// without creating a second event sequence.
type TurnRef struct {
	TurnID    string
	SessionID string
	Replayed  bool
}

// IngressSink is the only handle a channel adapter has to run work. It is
// handed to ChannelPort.Start; the adapter can submit envelopes but cannot
// construct its own turn loop. The Engine is the sole implementation.
type IngressSink interface {
	Accept(ctx context.Context, env *IngressEnvelope) (TurnRef, error)
}
