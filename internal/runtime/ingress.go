package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

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

var _ IngressSink = (*Engine)(nil)

// Accept is the ingress entry point every gateway, webhook, and scheduler
// uses. It validates the envelope's identity, claims the dedupe key
// (source, external_id) with the accepted event, and enqueues the turn; the
// turn then runs to a durable terminal independent of the caller. A duplicate
// delivery returns the original turn reference and creates no second event
// sequence.
func (e *Engine) Accept(ctx context.Context, env *IngressEnvelope) (TurnRef, error) {
	if env == nil {
		return TurnRef{}, invalidArgument("ingress envelope must not be nil")
	}
	req, err := turnRequestFromEnvelope(env)
	if err != nil {
		return TurnRef{}, err
	}
	accepted, originalTurnID, replay, err := e.claim(ctx, req)
	if err != nil {
		return TurnRef{}, err
	}
	if replay {
		e.releasePending()
		return TurnRef{TurnID: originalTurnID, SessionID: req.SessionID, Replayed: true}, nil
	}
	e.enqueue(ctx, req, &accepted, nil)
	return TurnRef{TurnID: req.TurnID, SessionID: req.SessionID}, nil
}

func turnRequestFromEnvelope(env *IngressEnvelope) (*TurnRequest, error) {
	var problems []error
	if env.Source == "" {
		problems = append(problems, invalidArgument("ingress source must not be empty"))
	}
	if env.ExternalID == "" {
		problems = append(problems, invalidArgument("ingress external id must not be empty"))
	}
	if env.PrincipalID == "" {
		problems = append(problems, invalidArgument("ingress principal must not be empty"))
	}
	if env.ConversationID == "" {
		problems = append(problems, invalidArgument("ingress conversation must not be empty"))
	}
	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	return &TurnRequest{
		SessionID:      env.ConversationID,
		PrincipalID:    env.PrincipalID,
		Origin:         Origin(env.Source),
		Parts:          env.Parts,
		IdempotencyKey: env.ExternalID,
		TraceParent:    env.TraceParent,
	}, nil
}
