package config

import (
	"errors"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/principal"
)

const (
	MaxStandingGrantTTL = 720 * time.Hour
	MinRetention        = 24 * time.Hour
)

func ValidateAccess(access *Access) error {
	if access == nil {
		return nil
	}
	var problems []error
	if timeout := time.Duration(access.ApprovalTimeout); timeout < principal.MinApprovalTimeout || timeout > principal.MaxApprovalTimeout {
		problems = append(problems, configkit.Invalid("access.approval_timeout", "must be between 1m and 24h"))
	}
	if ttl := time.Duration(access.StandingGrantTTL); ttl <= 0 || ttl > MaxStandingGrantTTL {
		problems = append(problems, configkit.Invalid("access.standing_grant_ttl", "must be positive and at most 720h"))
	}
	if timeout := time.Duration(access.HandoffTimeout); timeout < principal.MinApprovalTimeout || timeout > MaxStandingGrantTTL {
		problems = append(problems, configkit.Invalid("access.handoff_timeout", "must be between 1m and 720h"))
	}
	for _, retention := range []struct {
		class string
		ttl   configkit.Duration
	}{{string(principal.ClassTrusted), access.Retention.Trusted}, {string(principal.ClassPublic), access.Retention.Public}} {
		if time.Duration(retention.ttl) < MinRetention {
			problems = append(problems, configkit.Invalid("access.retention."+retention.class, "must be at least 24h"))
		}
	}
	for _, budget := range []struct {
		class  string
		limits PrincipalBudget
	}{{string(principal.ClassTrusted), access.Budgets.Trusted}, {string(principal.ClassPublic), access.Budgets.Public}} {
		if budget.limits.MaxTokensPerDay < 1 {
			problems = append(problems, configkit.Invalid("access.budgets."+budget.class+".max_tokens_per_day", "must be at least 1"))
		}
		if budget.limits.MaxTurnsPerMinute < 1 {
			problems = append(problems, configkit.Invalid("access.budgets."+budget.class+".max_turns_per_minute", "must be at least 1"))
		}
	}
	return errors.Join(problems...)
}
