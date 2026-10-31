package runtimeengine

import (
	"context"
	"errors"
	"strings"

	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/runtime/ingress"
)

var _ runtimeingress.IngressSink = (*Engine)(nil)

func (e *Engine) Accept(ctx context.Context, env *runtimeingress.IngressEnvelope) (runtimeingress.TurnRef, error) {
	if env == nil {
		return runtimeingress.TurnRef{}, invalidArgument("ingress envelope must not be nil")
	}
	req, err := turnRequestFromEnvelope(env)
	if err != nil {
		return runtimeingress.TurnRef{}, err
	}
	accepted, originalTurnID, replay, granted, err := e.claim(ctx, req)
	if err != nil {
		return runtimeingress.TurnRef{}, err
	}
	if replay {
		e.releasePending()
		return runtimeingress.TurnRef{TurnID: originalTurnID, SessionID: req.SessionID, Replayed: true}, nil
	}
	if e.sessionStore != nil {
		e.stageDurable(ctx, req, &accepted, nil, granted)
		return runtimeingress.TurnRef{TurnID: req.TurnID, SessionID: req.SessionID}, nil
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
	if env.AgentID == "" {
		problems = append(problems, invalidArgument("ingress persona must not be empty"))
	}
	if env.ConversationID == "" {
		problems = append(problems, invalidArgument("ingress conversation must not be empty"))
	} else if env.AgentID != "" && env.Source != "" && !strings.HasPrefix(env.ConversationID, runtimeingress.SessionKey(env.AgentID, env.Source, "")) {
		problems = append(problems, invalidArgument("ingress conversation must be scoped to its persona and source"))
	}
	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	return &runtime.TurnRequest{
		SessionID:      env.ConversationID,
		PrincipalID:    env.PrincipalID,
		PrincipalClass: principal.ParseClass(env.PrincipalClass),
		Origin:         runtime.Origin(env.Source),
		Parts:          env.Parts,
		ReplyContext:   env.ReplyContext,
		IdempotencyKey: env.ExternalID,
		TraceParent:    env.TraceParent,
		AgentID:        env.AgentID,
		Skill:          env.Skill,
	}, nil
}
