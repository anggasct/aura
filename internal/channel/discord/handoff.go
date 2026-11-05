package discord

import (
	"context"
	"net/http"
	"strings"

	"github.com/anggasct/aura/internal/channel/access"
	"github.com/anggasct/aura/internal/channel/status"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
)

const (
	HandoffStateNone    = access.HandoffStateNone
	HandoffStateOpen    = access.HandoffStateOpen
	HandoffStateExpired = access.HandoffStateExpired
	handoffForwardRunes = 1500
)

type HandoffGate = access.HandoffGate

func (a *Adapter) NotifyHandoff(ctx context.Context, sessionID, reason string) (access.HandoffTarget, error) {
	binding, ok := a.lookupTarget(sessionID)
	if !ok {
		return access.HandoffTarget{}, access.ErrNoHandoffTarget
	}
	token, err := a.tokenRef.Resolve()
	if err != nil {
		return access.HandoffTarget{}, Errorf(ErrorCodeConnectionFailed, "bot token is unavailable")
	}
	dm, err := a.ownerDM(ctx, token)
	if err != nil {
		return access.HandoffTarget{}, err
	}
	definition, _ := a.personaDefinition(binding.persona)
	notice := access.HandoffOwnerNotice(personaLabel(&definition), sessionID, binding.principal, reason)
	if err := sendText(ctx, a.httpClient, a.restBase, token, dm, notice); err != nil {
		return access.HandoffTarget{}, err
	}
	a.postNotice(ctx, token, binding.channelID, definition.Message(config.MessageHandoffOpen))
	return access.HandoffTarget{Channel: principal.ChannelDiscord, Account: a.account.Name, ChannelID: binding.channelID}, nil
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
		text := "handoff · session " + envelope.ConversationID + "\n" + envelope.PrincipalID + ": " + status.TruncateRunes(strings.TrimSpace(msg.Content), handoffForwardRunes)
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

func (a *Adapter) ReplyTarget(sessionID string) (access.HandoffTarget, bool) {
	binding, ok := a.lookupTarget(sessionID)
	if !ok || binding.channelID == "" {
		return access.HandoffTarget{}, false
	}
	return access.HandoffTarget{Channel: principal.ChannelDiscord, Account: a.account.Name, ChannelID: binding.channelID}, true
}
