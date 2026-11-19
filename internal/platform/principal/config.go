package principal

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
)

const ownerKey = "owner"

const (
	MaxStandingGrantTTL = 720 * time.Hour
	MinRetention        = 24 * time.Hour
)

var ownerFixedKeys = []string{"name", "language", "timezone"}

type Config struct {
	ApprovalTimeout  configkit.Duration `json:"approval_timeout"`
	StandingGrantTTL configkit.Duration `json:"standing_grant_ttl"`
	Budgets          Budgets            `json:"budgets"`
	Retention        Retention          `json:"retention"`
	HandoffTimeout   configkit.Duration `json:"handoff_timeout"`
}

type Retention struct {
	Trusted configkit.Duration `json:"trusted"`
	Public  configkit.Duration `json:"public"`
}

type Budgets struct {
	Trusted Budget `json:"trusted"`
	Public  Budget `json:"public"`
}

type Budget struct {
	MaxTokensPerDay   int64 `json:"max_tokens_per_day"`
	MaxTurnsPerMinute int   `json:"max_turns_per_minute"`
}

type Owner struct {
	Name        string            `json:"name,omitempty"`
	Language    string            `json:"language,omitempty"`
	Timezone    string            `json:"timezone,omitempty"`
	Preferences map[string]string `json:"preferences,omitempty"`
	Identities  []string          `json:"identities,omitempty"`
}

func Default() Config {
	return Config{
		ApprovalTimeout:  configkit.Duration(30 * time.Minute),
		StandingGrantTTL: configkit.Duration(168 * time.Hour),
		Budgets: Budgets{
			Trusted: Budget{MaxTokensPerDay: 200000, MaxTurnsPerMinute: 6},
			Public:  Budget{MaxTokensPerDay: 20000, MaxTurnsPerMinute: 3},
		},
		Retention:      Retention{Trusted: configkit.Duration(4320 * time.Hour), Public: configkit.Duration(720 * time.Hour)},
		HandoffTimeout: configkit.Duration(24 * time.Hour),
	}
}

func (c Config) Validate() error {
	var problems []error
	if timeout := time.Duration(c.ApprovalTimeout); timeout < MinApprovalTimeout || timeout > MaxApprovalTimeout {
		problems = append(problems, configkit.Invalid("approval_timeout", "must be between 1m and 24h"))
	}
	if ttl := time.Duration(c.StandingGrantTTL); ttl <= 0 || ttl > MaxStandingGrantTTL {
		problems = append(problems, configkit.Invalid("standing_grant_ttl", "must be positive and at most 720h"))
	}
	if timeout := time.Duration(c.HandoffTimeout); timeout < MinApprovalTimeout || timeout > MaxStandingGrantTTL {
		problems = append(problems, configkit.Invalid("handoff_timeout", "must be between 1m and 720h"))
	}
	for _, retention := range []struct {
		class string
		ttl   configkit.Duration
	}{{string(ClassTrusted), c.Retention.Trusted}, {string(ClassPublic), c.Retention.Public}} {
		if time.Duration(retention.ttl) < MinRetention {
			problems = append(problems, configkit.Invalid("retention."+retention.class, "must be at least 24h"))
		}
	}
	for _, budget := range []struct {
		class  string
		limits Budget
	}{{string(ClassTrusted), c.Budgets.Trusted}, {string(ClassPublic), c.Budgets.Public}} {
		if budget.limits.MaxTokensPerDay < 1 {
			problems = append(problems, configkit.Invalid("budgets."+budget.class+".max_tokens_per_day", "must be at least 1"))
		}
		if budget.limits.MaxTurnsPerMinute < 1 {
			problems = append(problems, configkit.Invalid("budgets."+budget.class+".max_turns_per_minute", "must be at least 1"))
		}
	}
	return errors.Join(problems...)
}

func ValidateOwner(owner *Owner) error {
	if owner == nil {
		return nil
	}
	return errors.Join(ownerValueProblems(owner), ownerIdentityProblem(owner))
}

func ownerValueProblems(owner *Owner) error {
	var problems []error
	for _, field := range []struct {
		path  string
		value string
	}{
		{"name", owner.Name},
		{"language", owner.Language},
		{"timezone", owner.Timezone},
	} {
		if err := validateOwnerValue(field.path, field.value); err != nil {
			problems = append(problems, err)
		}
	}
	if owner.Timezone != "" {
		if _, err := time.LoadLocation(owner.Timezone); err != nil {
			problems = append(problems, configkit.Invalid("timezone", "is not a valid IANA timezone"))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(owner.Preferences)) {
		switch {
		case key == "":
			problems = append(problems, configkit.Invalid("preferences", "keys must not be empty"))
			continue
		case key != strings.TrimSpace(key):
			problems = append(problems, configkit.Invalid("preferences", fmt.Sprintf("key %q must not carry surrounding whitespace", key)))
			continue
		case strings.Contains(key, ":"):
			problems = append(problems, configkit.Invalid("preferences", fmt.Sprintf("key %q must not contain a colon", key)))
			continue
		case slices.Contains(ownerFixedKeys, key):
			problems = append(problems, configkit.Invalid("preferences", fmt.Sprintf("key %q collides with %s.%s", key, ownerKey, key)))
			continue
		}
		if err := validateOwnerValue("preferences."+key, owner.Preferences[key]); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}

func ownerIdentityProblem(owner *Owner) error {
	if len(owner.Identities) == 0 {
		return nil
	}
	if _, err := NewResolver(owner.Identities); err != nil {
		return configkit.Field("identities", fault.New(ErrorCodeIdentityInvalid, identityProblemDetail(err)))
	}
	return nil
}

func identityProblemDetail(err error) string {
	problems := []error{err}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		problems = joined.Unwrap()
	}
	lines := make([]string, 0, len(problems))
	for _, problem := range problems {
		var typed *fault.Error[ErrorCode]
		if errors.As(problem, &typed) {
			lines = append(lines, strings.Replace(problem.Error(), typed.Error(), typed.Detail, 1))
			continue
		}
		lines = append(lines, problem.Error())
	}
	return strings.Join(lines, "\n")
}

func validateOwnerValue(path, value string) error {
	if value == "" {
		return nil
	}
	if strings.ContainsAny(value, "\r\n") {
		return configkit.Invalid(path, "must be a single line")
	}
	if value != strings.TrimSpace(value) {
		return configkit.Invalid(path, "must not carry surrounding whitespace")
	}
	return nil
}
