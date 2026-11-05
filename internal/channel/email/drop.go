package email

import (
	"context"
	"log/slog"

	"github.com/anggasct/aura/internal/channel/access"
)

type dropReason string

const (
	dropAutomatedMail         dropReason = "automated_mail"
	dropSenderInvalid         dropReason = "sender_invalid"
	dropSenderUnauthenticated dropReason = "sender_unauthenticated"
	dropMessageTooLarge       dropReason = "message_too_large"
	dropMessageMalformed      dropReason = "message_malformed"
	dropMessageUnavailable    dropReason = "message_unavailable"
	dropIntakeRejected        dropReason = "intake_rejected"
	dropAuthorNotAllowed      dropReason = access.DropAuthorNotAllowed
	dropEmptyContent          dropReason = access.DropEmptyContent
	dropPersonaUnrouted       dropReason = access.DropPersonaUnrouted
	dropAudience              dropReason = access.DropAudience
	dropPersonaEvalMissing    dropReason = access.DropPersonaEvalMissing
	dropRateLimited           dropReason = access.DropRateLimited
	dropBudgetExhausted       dropReason = access.DropBudgetExhausted
	dropBudgetCheckFailed     dropReason = access.DropBudgetCheckFailed
)

func (a *Adapter) SetDropObserver(observe func(ctx context.Context, reason string)) {
	if a == nil {
		return
	}
	a.dropped = observe
}

func (a *Adapter) drop(ctx context.Context, uid uint32, reason dropReason, known bool) {
	level := slog.LevelDebug
	if known {
		level = slog.LevelInfo
	}
	a.logger.Log(ctx, level, "email message dropped", "component", "email", "account", a.account.Name, "reason", string(reason), "uid", uid)
	if a.dropped != nil {
		a.dropped(ctx, string(reason))
	}
}
