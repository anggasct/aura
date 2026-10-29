package cli

import (
	"context"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/principal"
)

const remediationMapOwnerIdentity = "add your discord user id to owner.identities as discord:<id>, then run aura owner rekey"

func ownerResolver(cfg *config.Config) (*principal.Resolver, error) {
	var identities []string
	if cfg != nil && cfg.Owner != nil {
		identities = cfg.Owner.Identities
	}
	return principal.NewResolver(identities)
}

type identityMappingCheck struct {
	owners *principal.Resolver
}

func (c identityMappingCheck) Check(context.Context) []health.Finding {
	if c.owners.Count(principal.ChannelDiscord) > 0 {
		return nil
	}
	return []health.Finding{{
		Component:   "principal",
		Code:        "principal_identities_unmapped",
		Status:      health.StatusDegraded,
		Severity:    health.SeverityWarning,
		Detail:      "discord is enabled but no owner identity is mapped; your discord turns run as a trusted user without owner context",
		Remediation: remediationMapOwnerIdentity,
	}}
}
