package email

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/channels/kit/media"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/principal"
)

type HandoffNotifier interface {
	NotifyOwner(ctx context.Context, requester *approval.Requester, sessionID, reason string) error
}

type MediaStore interface {
	Put(ctx context.Context, content io.Reader, meta *media.Meta) (media.Receipt, error)
}

func (a *Adapter) servesPublic() bool {
	return a.router != nil && a.router.ServesPublic(auraagent.BindingChannelEmail, a.account.Name)
}

func (a *Adapter) handoffDrop(ctx context.Context, found *admission) dropReason {
	if a.access.Handoffs == nil || found.class == principal.ClassOwner {
		return ""
	}
	state, err := a.access.Handoffs.Check(ctx, found.session)
	if err != nil {
		a.logger.WarnContext(ctx, "handoff check failed", "component", "email", "account", a.account.Name)
		return dropHandoffCheckFailed
	}
	if state == access.HandoffStateOpen {
		return dropHandoffHeld
	}
	return ""
}

func (a *Adapter) forgetMeRequest(body string) (command.Invocation, bool) {
	if a.access.Erasure == nil {
		return command.Invocation{}, false
	}
	inv, ok := command.Parse(strings.TrimSpace(body))
	if !ok || inv.Name != command.NameForgetMe || strings.ContainsAny(strings.TrimSpace(body), "\r\n") {
		return command.Invocation{}, false
	}
	switch {
	case len(inv.Args) == 0:
		return inv, true
	case len(inv.Args) == 1 && strings.EqualFold(inv.Args[0], command.ArgConfirm):
		return inv, true
	default:
		return command.Invocation{}, false
	}
}

func (a *Adapter) forgetMe(ctx context.Context, inv *command.Invocation, found *admission) {
	definition, _ := a.personaDefinition(found.persona)
	switch {
	case found.class == principal.ClassOwner:
		a.noticeSender(ctx, found, definition.Message(auraagent.MessageNotPermitted))
	case len(inv.Args) == 0:
		a.noticeSender(ctx, found, definition.Message(auraagent.MessageForgetmeConfirm))
	default:
		if err := a.access.Erasure.Forget(ctx, found.principal); err != nil {
			a.logger.WarnContext(ctx, "forgetme erasure failed", "component", "email", "account", a.account.Name)
			if errors.Is(err, access.ErrForgetRunsStopping) {
				a.noticeSender(ctx, found, definition.Message(auraagent.MessageForgetmeStopping))
			}
			return
		}
		a.noticeSender(ctx, found, definition.Message(auraagent.MessageForgetmeDone))
	}
}
