package cli

import (
	"context"
	"errors"

	"github.com/anggasct/aura/internal/channel/access"
	"github.com/anggasct/aura/internal/channel/approval"
	"github.com/anggasct/aura/internal/principal"
)

var errNoOwnerChannel = errors.New("handoff: no channel can reach the owner")

type ownerHandoffNotifier interface {
	personaServer
	NotifyOwnerHandoff(ctx context.Context, requester *approval.Requester, sessionID, reason string) error
}

type ownerHandoffRouter struct {
	notifiers []ownerHandoffNotifier
}

func (r *ownerHandoffRouter) NotifyOwner(ctx context.Context, requester *approval.Requester, sessionID, reason string) error {
	unreachable := errNoOwnerChannel
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

type httpRequesters interface {
	RequesterOf(sessionID string) (approval.Requester, bool)
}

type httpHandoffNotifier struct {
	requesters httpRequesters
	owners     *ownerHandoffRouter
}

func newHTTPHandoffNotifier(requesters httpRequesters, adapters []approvalAdapter) *httpHandoffNotifier {
	notifiers := make([]ownerHandoffNotifier, 0, len(adapters))
	for _, adapter := range adapters {
		notifiers = append(notifiers, adapter)
	}
	return &httpHandoffNotifier{requesters: requesters, owners: &ownerHandoffRouter{notifiers: notifiers}}
}

func (n *httpHandoffNotifier) NotifyHandoff(ctx context.Context, sessionID, reason string) (access.HandoffTarget, error) {
	requester, held := n.requesters.RequesterOf(sessionID)
	if !held {
		return access.HandoffTarget{}, access.ErrNoHandoffTarget
	}
	channel, keyID, err := principal.ParseIdentity(requester.Principal)
	if err != nil || channel != principal.ChannelHTTP {
		return access.HandoffTarget{}, errors.New("handoff: the session requester is not an http key principal")
	}
	if err := n.owners.NotifyOwner(ctx, &requester, sessionID, reason); err != nil {
		return access.HandoffTarget{}, err
	}
	return access.HandoffTarget{Channel: principal.ChannelHTTP, Account: keyID}, nil
}
