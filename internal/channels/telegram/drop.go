package telegram

import (
	"context"
	"log/slog"

	"github.com/anggasct/aura/internal/channels/kit/access"
)

type dropReason string

const (
	dropAuthorMissing        dropReason = "author_missing"
	dropAuthorBot            dropReason = access.DropAuthorBot
	dropChatNotSupported     dropReason = "chat_not_supported"
	dropChatNotAllowed       dropReason = "chat_not_allowed"
	dropBotIdentityUnknown   dropReason = "bot_identity_unknown"
	dropNotMentioned         dropReason = access.DropNotMentioned
	dropEmptyContent         dropReason = access.DropEmptyContent
	dropAuthorNotAllowed     dropReason = access.DropAuthorNotAllowed
	dropPrivateChatsDisabled dropReason = "private_chats_disabled"
	dropPairing              dropReason = access.DropPairing
	dropPersonaUnrouted      dropReason = access.DropPersonaUnrouted
	dropAudience             dropReason = access.DropAudience
	dropPersonaEvalMissing   dropReason = access.DropPersonaEvalMissing
	dropRateLimited          dropReason = access.DropRateLimited
	dropBudgetExhausted      dropReason = access.DropBudgetExhausted
	dropBudgetCheckFailed    dropReason = access.DropBudgetCheckFailed
	dropHandoffCheckFailed   dropReason = "handoff_check_failed"
	dropHandoffForwardFailed dropReason = "handoff_forward_failed"
	dropCallbackRejected     dropReason = "callback_rejected"
	dropAttachmentFailed     dropReason = "attachment_failed"
)

const (
	dropScopePrivate = "private"
	dropScopeChat    = "chat"
)

func (a *Adapter) SetDropObserver(observe func(ctx context.Context, reason string)) {
	if a == nil {
		return
	}
	a.dropped = observe
}

func messageScope(msg *message) string {
	if msg.Chat.Type == chatTypePrivate {
		return dropScopePrivate
	}
	return dropScopeChat
}

func (a *Adapter) drop(ctx context.Context, scope string, reason dropReason, known bool) {
	level := slog.LevelDebug
	if known {
		level = slog.LevelInfo
	}
	a.logger.Log(ctx, level, "telegram message dropped", "component", "telegram", "account", a.account.Name, "reason", string(reason), "scope", scope)
	if a.dropped != nil {
		a.dropped(ctx, string(reason))
	}
}
