package telegram

import (
	"context"
	"strconv"
	"strings"

	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/channels/kit/approval"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
)

func (a *Adapter) NotifyHandoff(ctx context.Context, sessionID, reason string) (access.HandoffTarget, error) {
	requester, ok := a.lookupTarget(sessionID)
	if !ok {
		return access.HandoffTarget{}, access.ErrNoHandoffTarget
	}
	owner := approval.Requester{Principal: requester.binding.Principal, Class: requester.binding.Class, Persona: requester.binding.Persona}
	if err := a.NotifyOwnerHandoff(ctx, &owner, sessionID, reason); err != nil {
		return access.HandoffTarget{}, err
	}
	definition, _ := a.personaDefinition(requester.binding.Persona)
	a.postNotice(ctx, requester.dest, definition.Message(auraagent.MessageHandoffOpen))
	return a.handoffTarget(&requester), nil
}

func (a *Adapter) NotifyOwnerHandoff(ctx context.Context, requester *approval.Requester, sessionID, reason string) error {
	if ctx == nil {
		return fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if requester == nil || requester.Principal == "" || requester.Persona == "" || sessionID == "" {
		return fault.New(ErrorCodeInvalidArgument, "handoff requester or session is missing")
	}
	ownerChat, ok := a.ownerChatID()
	if !ok {
		return fault.New(ErrorCodeHandoffUnavailable, "owner has no telegram identity")
	}
	token, ok := a.botToken()
	if !ok {
		return fault.New(ErrorCodeConnectionFailed, "bot token is unavailable")
	}
	definition, _ := a.personaDefinition(requester.Persona)
	if definition.ID == "" {
		definition.ID = requester.Persona
	}
	notice := access.HandoffOwnerNotice(approval.PersonaLabel(definition.ID, definition.Name), sessionID, requester.Principal, reason)
	return sendPlainText(ctx, a.bot, token, destination{chatID: ownerChat}, notice)
}

func (a *Adapter) ReplyTarget(sessionID string) (access.HandoffTarget, bool) {
	requester, ok := a.lookupTarget(sessionID)
	if !ok || requester.dest.chatID == 0 {
		return access.HandoffTarget{}, false
	}
	return a.handoffTarget(&requester), true
}

func (a *Adapter) handoffTarget(requester *approvalTarget) access.HandoffTarget {
	target := access.HandoffTarget{
		Channel:   principal.ChannelTelegram,
		Account:   a.account.Name,
		ChannelID: strconv.FormatInt(requester.dest.chatID, 10),
	}
	if requester.dest.threadID != 0 {
		target.ThreadID = strconv.FormatInt(requester.dest.threadID, 10)
	}
	return target
}

func DeliverText(ctx context.Context, account *Account, chatID, threadID, text string) error {
	if ctx == nil {
		return fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if account == nil {
		return fault.New(ErrorCodeInvalidArgument, "telegram account must not be nil")
	}
	dest, err := parseDestination(chatID, threadID)
	if err != nil {
		return err
	}
	tokenRef, err := parseTokenRef(account.BotTokenRef)
	if err != nil {
		return err
	}
	token, err := tokenRef.Resolve()
	token = strings.TrimSpace(token)
	if err != nil || token == "" {
		return fault.New(ErrorCodeConnectionFailed, "bot token is unavailable")
	}
	return sendPlainText(ctx, newBotClient(), token, dest, text)
}

func parseDestination(chatID, threadID string) (destination, error) {
	chatValue, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil || chatValue == 0 {
		return destination{}, fault.New(ErrorCodeInvalidArgument, "chat id is not a telegram chat id")
	}
	if threadID == "" {
		return destination{chatID: chatValue}, nil
	}
	thread, err := strconv.ParseInt(threadID, 10, 64)
	if err != nil || thread <= 0 {
		return destination{}, fault.New(ErrorCodeInvalidArgument, "thread id is not a telegram topic id")
	}
	return destination{chatID: chatValue, threadID: thread}, nil
}

func sendPlainText(ctx context.Context, bot *botClient, token string, dest destination, text string) error {
	if strings.TrimSpace(text) == "" {
		return fault.New(ErrorCodeInvalidArgument, "text must not be empty")
	}
	for _, chunk := range splitMessage(text) {
		call, cancel := context.WithTimeout(ctx, sendTimeout)
		_, err := bot.sendMessage(call, token, &sendMessageRequest{ChatID: dest.chatID, MessageThreadID: dest.threadID, Text: chunk, LinkPreviewOptions: previewsDisabled()})
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}
