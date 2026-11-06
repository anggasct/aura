package cli

import (
	"context"

	"github.com/anggasct/aura/internal/channel/approval"
	"github.com/anggasct/aura/internal/channel/discord"
	"github.com/anggasct/aura/internal/channel/telegram"
)

type ownerApprover interface {
	ServesPersona(persona string) bool
	DecideForOwner(ctx context.Context, prompt *approval.Prompt, requester *approval.Requester) (bool, error)
}

type approvalAdapter interface {
	approvalChannel
	ownerApprover
	ownerHandoffNotifier
}

type personaServer interface {
	ServesPersona(persona string) bool
}

type ownerApprovalRouter struct {
	approvers []ownerApprover
	routed    func(ctx context.Context, result string)
}

var _ approval.OwnerApprover = (*ownerApprovalRouter)(nil)

func (r *ownerApprovalRouter) DecideForOwner(ctx context.Context, prompt *approval.Prompt, requester *approval.Requester) (bool, error) {
	if len(r.approvers) == 0 {
		if r.routed != nil {
			r.routed(ctx, approval.RoutedUnavailable)
		}
		return false, nil
	}
	var unavailable error
	for _, approver := range r.ordered(requester.Persona) {
		accepted, err := approver.DecideForOwner(ctx, prompt, requester)
		if err == nil || !approvalUnavailable(err) {
			return accepted, err
		}
		unavailable = err
	}
	return false, unavailable
}

func (r *ownerApprovalRouter) ordered(persona string) []ownerApprover {
	return servingFirst(r.approvers, persona)
}

func servingFirst[T personaServer](candidates []T, persona string) []T {
	serving := make([]T, 0, len(candidates))
	rest := make([]T, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.ServesPersona(persona) {
			serving = append(serving, candidate)
			continue
		}
		rest = append(rest, candidate)
	}
	return append(serving, rest...)
}

func approvalUnavailable(err error) bool {
	if code, ok := discord.CodeOf(err); ok && code == discord.ErrorCodeApprovalUnavailable {
		return true
	}
	code, ok := telegram.CodeOf(err)
	return ok && code == telegram.ErrorCodeApprovalUnavailable
}

func newOwnerRoute(adapters []approvalAdapter, routed func(ctx context.Context, result string)) (*approval.OwnerRoute, error) {
	approvers := make([]ownerApprover, 0, len(adapters))
	for _, adapter := range adapters {
		approvers = append(approvers, adapter)
	}
	return approval.NewOwnerRoute(&ownerApprovalRouter{approvers: approvers, routed: routed})
}

func approvalChannels(adapters []approvalAdapter, ownerRoute *approval.OwnerRoute) []approvalChannel {
	channels := make([]approvalChannel, 0, len(adapters)+1)
	for _, adapter := range adapters {
		channels = append(channels, adapter)
	}
	if ownerRoute != nil {
		channels = append(channels, ownerRoute)
	}
	return channels
}
