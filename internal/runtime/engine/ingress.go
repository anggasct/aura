package runtimeengine

import (
	"context"
	"errors"

	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/runtime/ingress"
)

var _ runtimeingress.IngressSink = (*Engine)(nil)

// Accept is the ingress entry point every gateway, webhook, and scheduler
// uses. It validates the envelope's identity, claims the dedupe key
// (source, external_id) with the accepted event, and enqueues the turn; the
// turn then runs to a durable terminal independent of the caller. A duplicate
// delivery returns the original turn reference and creates no second event
// sequence.
func (e *Engine) Accept(ctx context.Context, env *runtimeingress.IngressEnvelope) (runtimeingress.TurnRef, error) {
	if env == nil {
		return runtimeingress.TurnRef{}, invalidArgument("ingress envelope must not be nil")
	}
	req, err := turnRequestFromEnvelope(env)
	if err != nil {
		return runtimeingress.TurnRef{}, err
	}
	accepted, originalTurnID, replay, err := e.claim(ctx, req)
	if err != nil {
		return runtimeingress.TurnRef{}, err
	}
	if replay {
		e.releasePending()
		return runtimeingress.TurnRef{TurnID: originalTurnID, SessionID: req.SessionID, Replayed: true}, nil
	}
	e.enqueue(ctx, req, &accepted, nil)
	return runtimeingress.TurnRef{TurnID: req.TurnID, SessionID: req.SessionID}, nil
}

func turnRequestFromEnvelope(env *runtimeingress.IngressEnvelope) (*runtime.TurnRequest, error) {
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
	return &runtime.TurnRequest{
		SessionID:      env.ConversationID,
		PrincipalID:    env.PrincipalID,
		Origin:         runtime.Origin(env.Source),
		Parts:          env.Parts,
		IdempotencyKey: env.ExternalID,
		TraceParent:    env.TraceParent,
	}, nil
}
