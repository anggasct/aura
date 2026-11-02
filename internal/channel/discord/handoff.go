package discord

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
)

const (
	HandoffStateNone    = "none"
	HandoffStateOpen    = "open"
	HandoffStateExpired = "expired"
	handoffForwardRunes = 1500
)

var ErrNoHandoffTarget = errors.New("discord: session has no known requester on this account")

type HandoffGate interface {
	Check(ctx context.Context, sessionID string) (string, error)
}

type HandoffTarget struct {
	Account   string
	ChannelID string
}

func (a *Adapter) NotifyHandoff(ctx context.Context, sessionID, reason string) (HandoffTarget, error) {
	binding, ok := a.lookupTarget(sessionID)
	if !ok {
		return HandoffTarget{}, ErrNoHandoffTarget
	}
	token, err := a.tokenRef.Resolve()
	if err != nil {
		return HandoffTarget{}, Errorf(ErrorCodeConnectionFailed, "bot token is unavailable")
	}
	dm, err := a.ownerDM(ctx, token)
	if err != nil {
		return HandoffTarget{}, err
	}
	definition, _ := a.personaDefinition(binding.persona)
	lines := []string{
		"handoff requested",
		"persona: " + personaLabel(&definition),
		"session: " + sessionID,
		"principal: " + binding.principal,
		"reason: " + truncateRunes(strings.TrimSpace(reason), handoffForwardRunes),
		"reply: aura reply " + sessionID + " <text>",
		"release: aura handoff release " + sessionID,
	}
	if err := sendText(ctx, a.httpClient, a.restBase, token, dm, strings.Join(lines, "\n")); err != nil {
		return HandoffTarget{}, err
	}
	a.postNotice(ctx, token, binding.channelID, definition.Message(config.MessageHandoffOpen))
	return HandoffTarget{Account: a.account.Name, ChannelID: binding.channelID}, nil
}

func (a *Adapter) ownerDM(ctx context.Context, token string) (string, error) {
	owners := a.owners.OwnerExternalIDs(principal.ChannelDiscord)
	if len(owners) == 0 {
		return "", Errorf(ErrorCodeApprovalUnavailable, "owner has no discord identity")
	}
	return a.ownerDMChannel(ctx, token, owners[0])
}

func (a *Adapter) handoffHeld(ctx context.Context, msg *messagePayload, envelope *runtimeingress.IngressEnvelope) bool {
	if a.access.Handoffs == nil || principal.ParseClass(envelope.PrincipalClass) == principal.ClassOwner {
		return false
	}
	state, err := a.access.Handoffs.Check(ctx, envelope.ConversationID)
	if err != nil {
		a.logger.WarnContext(ctx, "handoff check failed", "component", "discord")
		return true
	}
	token, tokenErr := a.tokenRef.Resolve()
	switch state {
	case HandoffStateOpen:
		if tokenErr != nil {
			return true
		}
		dm, err := a.ownerDM(ctx, token)
		if err != nil {
			a.logger.WarnContext(ctx, "handoff forward failed", "component", "discord")
			return true
		}
		text := "handoff · session " + envelope.ConversationID + "\n" + envelope.PrincipalID + ": " + truncateRunes(strings.TrimSpace(msg.Content), handoffForwardRunes)
		if err := sendText(ctx, a.httpClient, a.restBase, token, dm, text); err != nil {
			a.logger.WarnContext(ctx, "handoff forward failed", "component", "discord")
		}
		return true
	case HandoffStateExpired:
		if tokenErr == nil {
			definition, _ := a.personaDefinition(envelope.AgentID)
			a.postNotice(ctx, token, msg.ChannelID, definition.Message(config.MessageHandoffClosed))
		}
		return false
	default:
		return false
	}
}

func DeliverText(ctx context.Context, account *config.DiscordAccount, channelID, text string) error {
	if account == nil {
		return Errorf(ErrorCodeInvalidArgument, "discord account must not be nil")
	}
	tokenRef, err := parseTokenRef(account.BotTokenRef)
	if err != nil {
		return err
	}
	token, err := tokenRef.Resolve()
	if err != nil {
		return Errorf(ErrorCodeConnectionFailed, "bot token is unavailable")
	}
	return sendText(ctx, &http.Client{Timeout: restTimeout}, discordRestBase, token, channelID, text)
}

func (a *Adapter) ReplyTarget(sessionID string) (HandoffTarget, bool) {
	binding, ok := a.lookupTarget(sessionID)
	if !ok || binding.channelID == "" {
		return HandoffTarget{}, false
	}
	return HandoffTarget{Account: a.account.Name, ChannelID: binding.channelID}, true
}
