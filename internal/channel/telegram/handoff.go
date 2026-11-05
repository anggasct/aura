package telegram

import (
	"context"
	"strconv"
	"strings"

	"github.com/anggasct/aura/internal/channel/access"
	"github.com/anggasct/aura/internal/channel/approval"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
)

func (a *Adapter) NotifyHandoff(ctx context.Context, sessionID, reason string) (access.HandoffTarget, error) {
	requester, ok := a.lookupTarget(sessionID)
	if !ok {
		return access.HandoffTarget{}, access.ErrNoHandoffTarget
	}
	ownerChat, ok := a.ownerChatID()
	if !ok {
		return access.HandoffTarget{}, Errorf(ErrorCodeHandoffUnavailable, "owner has no telegram identity")
	}
	token, ok := a.botToken()
	if !ok {
		return access.HandoffTarget{}, Errorf(ErrorCodeConnectionFailed, "bot token is unavailable")
	}
	definition, _ := a.personaDefinition(requester.binding.Persona)
	if definition.ID == "" {
		definition.ID = requester.binding.Persona
	}
	notice := access.HandoffOwnerNotice(approval.PersonaLabel(definition.ID, definition.Name), sessionID, requester.binding.Principal, reason)
	if err := sendPlainText(ctx, a.bot, token, destination{chatID: ownerChat}, notice); err != nil {
		return access.HandoffTarget{}, err
	}
	a.postNotice(ctx, requester.dest, definition.Message(config.MessageHandoffOpen))
	return a.handoffTarget(&requester), nil
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

func DeliverText(ctx context.Context, account *config.TelegramAccount, chatID, threadID, text string) error {
	if ctx == nil {
		return Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if account == nil {
		return Errorf(ErrorCodeInvalidArgument, "telegram account must not be nil")
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
		return Errorf(ErrorCodeConnectionFailed, "bot token is unavailable")
	}
	return sendPlainText(ctx, newBotClient(), token, dest, text)
}

func parseDestination(chatID, threadID string) (destination, error) {
	chatValue, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil || chatValue == 0 {
		return destination{}, Errorf(ErrorCodeInvalidArgument, "chat id is not a telegram chat id")
	}
	if threadID == "" {
		return destination{chatID: chatValue}, nil
	}
	thread, err := strconv.ParseInt(threadID, 10, 64)
	if err != nil || thread <= 0 {
		return destination{}, Errorf(ErrorCodeInvalidArgument, "thread id is not a telegram topic id")
	}
	return destination{chatID: chatValue, threadID: thread}, nil
}

func sendPlainText(ctx context.Context, bot *botClient, token string, dest destination, text string) error {
	if strings.TrimSpace(text) == "" {
		return Errorf(ErrorCodeInvalidArgument, "text must not be empty")
	}
	for _, chunk := range splitMessage(text) {
		call, cancel := context.WithTimeout(ctx, sendTimeout)
		_, err := bot.sendMessage(call, token, &sendMessageRequest{ChatID: dest.chatID, MessageThreadID: dest.threadID, Text: chunk})
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}
