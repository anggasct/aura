package principal

import (
	"context"

	"github.com/anggasct/aura/internal/platform/health"
)

const RemediationMapOwnerIdentity = "add your discord user id to owner.identities as discord:<id>, then run aura owner rekey"

type IdentityMappingCheck struct {
	Owners *Resolver
}

func (c IdentityMappingCheck) Check(context.Context) []health.Finding {
	if c.Owners.Count(ChannelDiscord) > 0 {
		return nil
	}
	return []health.Finding{{
		Component:   "principal",
		Code:        "principal_identities_unmapped",
		Status:      health.StatusDegraded,
		Severity:    health.SeverityWarning,
		Detail:      "discord is enabled but no owner identity is mapped; your discord turns run as a trusted user without owner context",
		Remediation: RemediationMapOwnerIdentity,
	}}
}
