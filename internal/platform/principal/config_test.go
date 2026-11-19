package principal

import (
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
)

func TestAccessConfigValidate(t *testing.T) {
	with := func(edit func(*Config)) Config {
		config := Default()
		edit(&config)
		return config
	}
	for _, test := range []struct {
		name   string
		config Config
		want   string
	}{
		{"default is valid", Default(), ""},
		{"approval timeout below the minimum", with(func(c *Config) { c.ApprovalTimeout = configkit.Duration(MinApprovalTimeout - time.Nanosecond) }), "approval_timeout: config_invalid: must be between 1m and 24h"},
		{"approval timeout at the minimum", with(func(c *Config) { c.ApprovalTimeout = configkit.Duration(MinApprovalTimeout) }), ""},
		{"approval timeout at the maximum", with(func(c *Config) { c.ApprovalTimeout = configkit.Duration(MaxApprovalTimeout) }), ""},
		{"approval timeout above the maximum", with(func(c *Config) { c.ApprovalTimeout = configkit.Duration(MaxApprovalTimeout + time.Nanosecond) }), "approval_timeout: config_invalid: must be between 1m and 24h"},
		{"standing grant ttl zero", with(func(c *Config) { c.StandingGrantTTL = 0 }), "standing_grant_ttl: config_invalid: must be positive and at most 720h"},
		{"standing grant ttl negative", with(func(c *Config) { c.StandingGrantTTL = configkit.Duration(-time.Hour) }), "standing_grant_ttl: config_invalid: must be positive and at most 720h"},
		{"standing grant ttl at the maximum", with(func(c *Config) { c.StandingGrantTTL = configkit.Duration(MaxStandingGrantTTL) }), ""},
		{"standing grant ttl above the maximum", with(func(c *Config) { c.StandingGrantTTL = configkit.Duration(MaxStandingGrantTTL + time.Nanosecond) }), "standing_grant_ttl: config_invalid: must be positive and at most 720h"},
		{"handoff timeout below the minimum", with(func(c *Config) { c.HandoffTimeout = configkit.Duration(MinApprovalTimeout - time.Nanosecond) }), "handoff_timeout: config_invalid: must be between 1m and 720h"},
		{"handoff timeout at the minimum", with(func(c *Config) { c.HandoffTimeout = configkit.Duration(MinApprovalTimeout) }), ""},
		{"handoff timeout above the approval maximum", with(func(c *Config) { c.HandoffTimeout = configkit.Duration(MaxApprovalTimeout + time.Hour) }), ""},
		{"handoff timeout at the maximum", with(func(c *Config) { c.HandoffTimeout = configkit.Duration(MaxStandingGrantTTL) }), ""},
		{"handoff timeout above the maximum", with(func(c *Config) { c.HandoffTimeout = configkit.Duration(MaxStandingGrantTTL + time.Nanosecond) }), "handoff_timeout: config_invalid: must be between 1m and 720h"},
		{"trusted retention below the floor", with(func(c *Config) { c.Retention.Trusted = configkit.Duration(MinRetention - time.Nanosecond) }), "retention.trusted: config_invalid: must be at least 24h"},
		{"trusted retention at the floor", with(func(c *Config) { c.Retention.Trusted = configkit.Duration(MinRetention) }), ""},
		{"public retention below the floor", with(func(c *Config) { c.Retention.Public = configkit.Duration(MinRetention - time.Nanosecond) }), "retention.public: config_invalid: must be at least 24h"},
		{"public retention at the floor", with(func(c *Config) { c.Retention.Public = configkit.Duration(MinRetention) }), ""},
		{"trusted tokens per day below one", with(func(c *Config) { c.Budgets.Trusted.MaxTokensPerDay = 0 }), "budgets.trusted.max_tokens_per_day: config_invalid: must be at least 1"},
		{"trusted turns per minute below one", with(func(c *Config) { c.Budgets.Trusted.MaxTurnsPerMinute = 0 }), "budgets.trusted.max_turns_per_minute: config_invalid: must be at least 1"},
		{"public tokens per day below one", with(func(c *Config) { c.Budgets.Public.MaxTokensPerDay = 0 }), "budgets.public.max_tokens_per_day: config_invalid: must be at least 1"},
		{"public turns per minute below one", with(func(c *Config) { c.Budgets.Public.MaxTurnsPerMinute = -1 }), "budgets.public.max_turns_per_minute: config_invalid: must be at least 1"},
		{"budgets at one are valid", with(func(c *Config) {
			c.Budgets = Budgets{Trusted: Budget{MaxTokensPerDay: 1, MaxTurnsPerMinute: 1}, Public: Budget{MaxTokensPerDay: 1, MaxTurnsPerMinute: 1}}
		}), ""},
		{"the zero config reports every rule in order", Config{}, strings.Join([]string{
			"approval_timeout: config_invalid: must be between 1m and 24h",
			"standing_grant_ttl: config_invalid: must be positive and at most 720h",
			"handoff_timeout: config_invalid: must be between 1m and 720h",
			"retention.trusted: config_invalid: must be at least 24h",
			"retention.public: config_invalid: must be at least 24h",
			"budgets.trusted.max_tokens_per_day: config_invalid: must be at least 1",
			"budgets.trusted.max_turns_per_minute: config_invalid: must be at least 1",
			"budgets.public.max_tokens_per_day: config_invalid: must be at least 1",
			"budgets.public.max_turns_per_minute: config_invalid: must be at least 1",
		}, "\n")},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.config.Validate()
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != test.want {
				t.Fatalf("Validate() = %q, want %q", got, test.want)
			}
			if test.want != "" && !fault.HasCode(err, configkit.ErrorCodeInvalid) {
				t.Fatalf("Validate() = %v, want the code %s", err, configkit.ErrorCodeInvalid)
			}
		})
	}
}

func TestOwnerValidate(t *testing.T) {
	for _, test := range []struct {
		name  string
		owner *Owner
		want  string
	}{
		{"nil owner is valid", nil, ""},
		{"empty owner is valid", &Owner{}, ""},
		{"a fully set owner is valid", &Owner{
			Name:        "Angga",
			Language:    "Indonesian",
			Timezone:    "Asia/Jakarta",
			Preferences: map[string]string{"style": "concise", "blank": ""},
			Identities:  []string{"discord:123", "telegram:5"},
		}, ""},
		{"name over several lines", &Owner{Name: "a\nb"}, "name: config_invalid: must be a single line"},
		{"name with a carriage return", &Owner{Name: "a\rb"}, "name: config_invalid: must be a single line"},
		{"name with surrounding whitespace", &Owner{Name: " Angga"}, "name: config_invalid: must not carry surrounding whitespace"},
		{"language over several lines", &Owner{Language: "id\nen"}, "language: config_invalid: must be a single line"},
		{"language with surrounding whitespace", &Owner{Language: "id "}, "language: config_invalid: must not carry surrounding whitespace"},
		{"timezone over several lines", &Owner{Timezone: "UTC\nUTC"}, "timezone: config_invalid: must be a single line\ntimezone: config_invalid: is not a valid IANA timezone"},
		{"timezone with surrounding whitespace", &Owner{Timezone: " UTC"}, "timezone: config_invalid: must not carry surrounding whitespace\ntimezone: config_invalid: is not a valid IANA timezone"},
		{"timezone that is not an IANA name", &Owner{Timezone: "Mars/Olympus"}, "timezone: config_invalid: is not a valid IANA timezone"},
		{"preference with an empty key", &Owner{Preferences: map[string]string{"": "x"}}, "preferences: config_invalid: keys must not be empty"},
		{"preference with an empty key skips the value rules", &Owner{Preferences: map[string]string{"": " x\n"}}, "preferences: config_invalid: keys must not be empty"},
		{"preference key with surrounding whitespace", &Owner{Preferences: map[string]string{" style": "x"}}, "preferences: config_invalid: key \" style\" must not carry surrounding whitespace"},
		{"preference key with a colon", &Owner{Preferences: map[string]string{"a:b": "x"}}, "preferences: config_invalid: key \"a:b\" must not contain a colon"},
		{"preference key colliding with name", &Owner{Preferences: map[string]string{"name": "x"}}, "preferences: config_invalid: key \"name\" collides with owner.name"},
		{"preference key colliding with language", &Owner{Preferences: map[string]string{"language": "x"}}, "preferences: config_invalid: key \"language\" collides with owner.language"},
		{"preference key colliding with timezone", &Owner{Preferences: map[string]string{"timezone": "x"}}, "preferences: config_invalid: key \"timezone\" collides with owner.timezone"},
		{"preference value over several lines", &Owner{Preferences: map[string]string{"style": "a\nb"}}, "preferences.style: config_invalid: must be a single line"},
		{"preference value with surrounding whitespace", &Owner{Preferences: map[string]string{"style": "x "}}, "preferences.style: config_invalid: must not carry surrounding whitespace"},
		{"preference problems follow the sorted keys", &Owner{Preferences: map[string]string{"zeta": " z", "alpha": "a\nb", "mid:dle": "m", "": "e"}}, strings.Join([]string{
			"preferences: config_invalid: keys must not be empty",
			"preferences.alpha: config_invalid: must be a single line",
			"preferences: config_invalid: key \"mid:dle\" must not contain a colon",
			"preferences.zeta: config_invalid: must not carry surrounding whitespace",
		}, "\n")},
		{"identity channel that is not supported", &Owner{Identities: []string{"slack:5550001"}}, "identities: owner_identity_invalid: owner.identities[0]: identity channel is not supported"},
		{"identity without a channel", &Owner{Identities: []string{"5550001"}}, "identities: owner_identity_invalid: owner.identities[0]: identity must be <channel>:<id>"},
		{"discord identity that is not numeric", &Owner{Identities: []string{"discord:owner5550002"}}, "identities: owner_identity_invalid: owner.identities[0]: discord identity must be a numeric user id"},
		{"http identity", &Owner{Identities: []string{"http:abcdefghijkl"}}, "identities: owner_identity_invalid: owner.identities[0]: http identities cannot be owner identities"},
		{"duplicate identity", &Owner{Identities: []string{"discord:5550003", "discord:5550003"}}, "identities: owner_identity_invalid: owner.identities[1]: identity is listed more than once"},
		{"every failing identity appears once", &Owner{Identities: []string{"slack:5550001", "discord:5550002", "discord:owner5550003"}}, "identities: owner_identity_invalid: owner.identities[0]: identity channel is not supported\nowner.identities[2]: discord identity must be a numeric user id"},
		{"value problems come before identity problems", &Owner{Name: " x", Identities: []string{"slack:1"}}, "name: config_invalid: must not carry surrounding whitespace\nidentities: owner_identity_invalid: owner.identities[0]: identity channel is not supported"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateOwner(test.owner)
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != test.want {
				t.Fatalf("ValidateOwner() = %q, want %q", got, test.want)
			}
			if strings.Contains(test.want, "config_invalid:") && !fault.HasCode(err, configkit.ErrorCodeInvalid) {
				t.Fatalf("ValidateOwner() = %v, want the code %s", err, configkit.ErrorCodeInvalid)
			}
			if strings.Contains(test.want, "owner_identity_invalid:") && !fault.HasCode(err, ErrorCodeIdentityInvalid) {
				t.Fatalf("ValidateOwner() = %v, want the code %s", err, ErrorCodeIdentityInvalid)
			}
		})
	}
}
