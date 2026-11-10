package discord

import (
	"context"
	"log/slog"

	"github.com/anggasct/aura/internal/channels/kit/access"
)

type dropReason string

const (
	dropAuthorBot          dropReason = access.DropAuthorBot
	dropMessageType        dropReason = "message_type"
	dropEmptyContent       dropReason = access.DropEmptyContent
	dropAuthorNotAllowed   dropReason = access.DropAuthorNotAllowed
	dropGuildNotAllowed    dropReason = "guild_not_allowed"
	dropChannelNotAllowed  dropReason = "channel_not_allowed"
	dropNotMentioned       dropReason = access.DropNotMentioned
	dropDMsDisabled        dropReason = "dms_disabled"
	dropPairing            dropReason = access.DropPairing
	dropThreadProbeFailed  dropReason = "thread_probe_failed"
	dropThreadForeign      dropReason = "thread_foreign"
	dropThreadUnresolved   dropReason = "thread_unresolved"
	dropPersonaUnrouted    dropReason = access.DropPersonaUnrouted
	dropAudience           dropReason = access.DropAudience
	dropPersonaEvalMissing dropReason = access.DropPersonaEvalMissing
	dropRateLimited        dropReason = access.DropRateLimited
	dropBudgetExhausted    dropReason = access.DropBudgetExhausted
	dropBudgetCheckFailed  dropReason = access.DropBudgetCheckFailed
	dropAttachmentFailed   dropReason = "attachment_failed"
)

const (
	dropScopeDM      = "dm"
	dropScopeChannel = "channel"
	dropScopeThread  = "thread"
)

func (a *Adapter) SetDropObserver(observe func(ctx context.Context, reason string)) {
	if a == nil {
		return
	}
	a.dropped = observe
}

func messageScope(msg *messagePayload) string {
	if msg.GuildID == "" {
		return dropScopeDM
	}
	return dropScopeChannel
}

func (a *Adapter) drop(ctx context.Context, msg *messagePayload, scope string, reason dropReason) (bool, error) {
	level := slog.LevelDebug
	if !msg.Author.Bot && msg.Author.ID != "" && msg.Author.ID != a.selfID {
		if _, _, known := resolveAuthor(a.account, a.owners, msg.Author.ID, a.authorAccess(ctx)); known {
			level = slog.LevelInfo
		}
	}
	a.logger.Log(ctx, level, "discord message dropped", "component", "discord", "account", a.account.Name, "reason", string(reason), "scope", scope)
	if a.dropped != nil {
		a.dropped(ctx, string(reason))
	}
	return false, nil
}
