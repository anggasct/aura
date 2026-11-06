package email

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/anggasct/aura/internal/channel/access"
	"github.com/anggasct/aura/internal/channel/approval"
	"github.com/anggasct/aura/internal/principal"
)

const maxTrackedSessions = 4096

type Route struct {
	Account string
	Sender  string
}

type ThreadReference struct {
	MessageID string `json:"message_id"`
	Subject   string `json:"subject"`
}

type sessionTarget struct {
	reply     replyReference
	requester approval.Requester
}

type sessionTargets struct {
	mu    sync.Mutex
	items map[string]sessionTarget
}

func (t *sessionTargets) remember(sessionID string, target *sessionTarget) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.items == nil {
		t.items = make(map[string]sessionTarget)
	}
	if _, known := t.items[sessionID]; !known && len(t.items) >= maxTrackedSessions {
		for drop := range t.items {
			delete(t.items, drop)
			break
		}
	}
	t.items[sessionID] = *target
}

func (t *sessionTargets) lookup(sessionID string) (sessionTarget, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	target, ok := t.items[sessionID]
	return target, ok
}

func (a *Adapter) rememberSession(found *admission) {
	var reply replyReference
	if err := json.Unmarshal(found.reply, &reply); err != nil {
		return
	}
	requester := approval.Requester{Principal: found.principal, Class: found.class, Persona: found.persona}
	a.targets.remember(found.session, &sessionTarget{reply: reply, requester: requester})
	a.approvals.Remember(found.session, &requester)
}

func (a *Adapter) SessionRoute(sessionID string) (Route, bool) {
	if a == nil {
		return Route{}, false
	}
	target, ok := a.targets.lookup(sessionID)
	if !ok {
		return Route{}, false
	}
	return Route{Account: a.account.Name, Sender: target.reply.Sender}, true
}

func (a *Adapter) NotifyHandoff(ctx context.Context, sessionID, reason string) (access.HandoffTarget, error) {
	target, ok := a.targets.lookup(sessionID)
	if !ok {
		return access.HandoffTarget{}, access.ErrNoHandoffTarget
	}
	if a.notifier == nil || target.requester.Class == principal.ClassOwner {
		return access.HandoffTarget{}, Errorf(ErrorCodeHandoffUnavailable, "no owner route can reach the owner")
	}
	if err := a.notifier.NotifyOwner(ctx, &target.requester, sessionID, reason); err != nil {
		return access.HandoffTarget{}, err
	}
	thread, err := json.Marshal(ThreadReference{MessageID: target.reply.MessageID, Subject: target.reply.Subject})
	if err != nil {
		return access.HandoffTarget{}, Errorf(ErrorCodeMessageInvalid, "thread reference is not encodable")
	}
	return access.HandoffTarget{
		Channel:   principal.ChannelEmail,
		Account:   a.account.Name,
		ChannelID: target.reply.Sender,
		ThreadID:  string(thread),
	}, nil
}
