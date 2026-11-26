package agent

import (
	"context"
	"time"

	"github.com/anggasct/aura/internal/platform/principal"
)

type AccessPolicy struct {
	registry            *Registry
	grants              principal.StandingGrantStore
	onStandingGrantUsed func(ctx context.Context)
	now                 func() time.Time
}

func NewAccessPolicy(registry *Registry, grants principal.StandingGrantStore, onStandingGrantUsed func(ctx context.Context)) *AccessPolicy {
	return &AccessPolicy{
		registry:            registry,
		grants:              grants,
		onStandingGrantUsed: onStandingGrantUsed,
		now:                 time.Now,
	}
}

func (p *AccessPolicy) WithClock(now func() time.Time) {
	if p != nil && now != nil {
		p.now = now
	}
}

func (p *AccessPolicy) ToolCeiling(agentID string, class principal.Class) ([]string, bool) {
	definition, ok := p.registry.Lookup(agentID)
	if !ok {
		return nil, false
	}
	ceiling, _ := definition.ToolCeiling(class)
	return ceiling, true
}

func (p *AccessPolicy) StandingGrant(ctx context.Context, principalID, agentID, toolName, server string) (bool, error) {
	definition, ok := p.registry.Lookup(agentID)
	if !ok || !definition.AllowsPrincipalTool(toolName, server) {
		return false, nil
	}
	_, active, err := p.grants.Active(ctx, principalID, agentID, toolName, p.now().UTC())
	if err != nil {
		return false, err
	}
	if active && p.onStandingGrantUsed != nil {
		p.onStandingGrantUsed(ctx)
	}
	return active, nil
}
