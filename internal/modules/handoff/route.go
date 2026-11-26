package handoff

import (
	"context"
	"errors"
	"slices"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
)

var ErrNoOwnerChannel = errors.New("handoff: no channel can reach the owner")

type OwnerRequester struct {
	Principal string
	Class     principal.Class
	Persona   string
}

type OwnerNotifier interface {
	ServesPersona(persona string) bool
	NotifyOwnerHandoff(ctx context.Context, requester *OwnerRequester, sessionID, reason string) error
}

type OwnerRouter struct {
	notifiers []OwnerNotifier
}

func NewOwnerRouter(notifiers []OwnerNotifier) *OwnerRouter {
	return &OwnerRouter{notifiers: slices.Clone(notifiers)}
}

func (r *OwnerRouter) NotifyOwner(ctx context.Context, requester *OwnerRequester, sessionID, reason string) error {
	if requester == nil {
		return fault.New(ErrorCodeInvalidArgument, "handoff requester must not be nil")
	}
	unreachable := ErrNoOwnerChannel
	for _, notifier := range servingFirst(r.notifiers, requester.Persona) {
		err := notifier.NotifyOwnerHandoff(ctx, requester, sessionID, reason)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		unreachable = err
	}
	return unreachable
}

func servingFirst(candidates []OwnerNotifier, persona string) []OwnerNotifier {
	serving := make([]OwnerNotifier, 0, len(candidates))
	rest := make([]OwnerNotifier, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.ServesPersona(persona) {
			serving = append(serving, candidate)
		} else {
			rest = append(rest, candidate)
		}
	}
	return append(serving, rest...)
}

type HTTPRequesterLookup interface {
	RequesterOf(sessionID string) (OwnerRequester, bool)
}

type HTTPNotifier struct {
	requesters HTTPRequesterLookup
	owners     *OwnerRouter
}

func NewHTTPNotifier(requesters HTTPRequesterLookup, owners *OwnerRouter) (*HTTPNotifier, error) {
	if requesters == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "handoff requester lookup must not be nil")
	}
	if owners == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "handoff owner router must not be nil")
	}
	return &HTTPNotifier{requesters: requesters, owners: owners}, nil
}

func (n *HTTPNotifier) NotifyHandoff(ctx context.Context, sessionID, reason string) (Target, error) {
	requester, held := n.requesters.RequesterOf(sessionID)
	if !held {
		return Target{}, ErrNoTarget
	}
	channel, keyID, err := principal.ParseIdentity(requester.Principal)
	if err != nil || channel != principal.ChannelHTTP {
		return Target{}, fault.New(ErrorCodeInvalidArgument, "handoff: the session requester is not an http key principal")
	}
	if err := n.owners.NotifyOwner(ctx, &requester, sessionID, reason); err != nil {
		return Target{}, err
	}
	return Target{Channel: principal.ChannelHTTP, Account: keyID}, nil
}
