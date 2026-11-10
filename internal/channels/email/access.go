package email

import (
	"context"
	"io"
	"strings"

	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/channels/kit/media"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/vision"
)

type HandoffNotifier interface {
	NotifyOwner(ctx context.Context, requester *approval.Requester, sessionID, reason string) error
}

type MediaStore interface {
	Put(ctx context.Context, content io.Reader, meta *media.Meta) (media.Receipt, error)
}

func (a *Adapter) SetOwnerRoute(route *approval.OwnerRoute) {
	if a == nil {
		return
	}
	a.approvals = route
}

func (a *Adapter) SetHandoffNotifier(notifier HandoffNotifier) {
	if a == nil {
		return
	}
	a.notifier = notifier
}

func (a *Adapter) SetMediaStore(store MediaStore) {
	if a == nil {
		return
	}
	a.artifacts = store
}

func (a *Adapter) SetVisionService(svc *vision.Service) {
	if a == nil {
		return
	}
	a.images.Store(svc)
}

func (a *Adapter) servesPublic() bool {
	return a.router != nil && a.router.ServesPublic(config.BindingChannelEmail, a.account.Name)
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
		a.noticeSender(ctx, found, definition.Message(config.MessageNotPermitted))
	case len(inv.Args) == 0:
		a.noticeSender(ctx, found, definition.Message(config.MessageForgetmeConfirm))
	default:
		if err := a.access.Erasure.Forget(ctx, found.principal); err != nil {
			a.logger.WarnContext(ctx, "forgetme erasure failed", "component", "email", "account", a.account.Name)
			return
		}
		a.noticeSender(ctx, found, definition.Message(config.MessageForgetmeDone))
	}
}
