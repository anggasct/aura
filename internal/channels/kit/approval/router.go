package approval

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
)

type PersonaServer interface {
	ServesPersona(persona string) bool
}

type PersonaOwnerApprover interface {
	PersonaServer
	OwnerApprover
}

type OwnerRouter struct {
	approvers []PersonaOwnerApprover
	routed    func(ctx context.Context, result string)
}

var _ OwnerApprover = (*OwnerRouter)(nil)

func NewOwnerRouter(approvers []PersonaOwnerApprover, routed func(ctx context.Context, result string)) *OwnerRouter {
	return &OwnerRouter{approvers: slices.Clone(approvers), routed: routed}
}

func (r *OwnerRouter) DecideForOwner(ctx context.Context, prompt *Prompt, requester *Requester) (bool, error) {
	if r == nil {
		return false, fault.New(ErrorCodeInvalidArgument, "owner approval router must not be nil")
	}
	if prompt == nil || requester == nil {
		return false, fault.New(ErrorCodeInvalidArgument, "approval prompt and requester must not be nil")
	}
	if len(r.approvers) == 0 {
		if r.routed != nil {
			r.routed(ctx, RoutedUnavailable)
		}
		return false, nil
	}
	var unavailable error
	for _, approver := range ServingFirst(r.approvers, requester.Persona) {
		accepted, err := approver.DecideForOwner(ctx, prompt, requester)
		if err == nil || !errors.Is(err, ErrUnavailable) {
			return accepted, err
		}
		unavailable = err
	}
	return false, unavailable
}

func ServingFirst[T PersonaServer](candidates []T, persona string) []T {
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

type WorkflowApprover struct {
	owners OwnerApprover
	now    func() time.Time
}

var _ OwnerApprover = (*WorkflowApprover)(nil)

func NewWorkflowApprover(owners OwnerApprover, now func() time.Time) *WorkflowApprover {
	if now == nil {
		now = time.Now
	}
	return &WorkflowApprover{owners: owners, now: now}
}

func (a *WorkflowApprover) DecideForOwner(ctx context.Context, prompt *Prompt, requester *Requester) (bool, error) {
	if a == nil || a.owners == nil {
		return false, fault.New(ErrorCodeInvalidArgument, "workflow owner approver must not be nil")
	}
	if prompt == nil || requester == nil {
		return false, fault.New(ErrorCodeInvalidArgument, "approval prompt and requester must not be nil")
	}
	card := *prompt
	card.StandingGrantAllowed = false
	card.ExpiresAt = prompt.ExpiresAt.Add(-prompt.Timeout)
	if !card.ExpiresAt.After(a.now()) {
		return false, nil
	}
	accepted, err := a.owners.DecideForOwner(ctx, &card, requester)
	if err != nil && (errors.Is(err, ErrUnavailable) || errors.Is(err, ErrTimeout)) {
		return false, nil
	}
	return accepted, err
}

func NewWorkflowRoute(approvers []PersonaOwnerApprover, routed func(ctx context.Context, result string)) (*OwnerRoute, error) {
	return NewOwnerRoute(NewWorkflowApprover(NewOwnerRouter(approvers, routed), time.Now))
}

func Targets(channels []Channel, routes ...*OwnerRoute) []Channel {
	targets := make([]Channel, 0, len(channels)+len(routes))
	targets = append(targets, channels...)
	for _, route := range routes {
		if route != nil {
			targets = append(targets, route)
		}
	}
	return targets
}
