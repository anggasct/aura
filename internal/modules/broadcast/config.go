package broadcast

import (
	"errors"
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
)

type QuietHours struct {
	Enabled bool   `json:"enabled"`
	Start   string `json:"start"`
	End     string `json:"end"`
}

type Config struct {
	Timezone       string             `json:"timezone"`
	QuietHours     QuietHours         `json:"quiet_hours"`
	MaxDigestItems int                `json:"max_digest_items"`
	MaxDigestBytes int64              `json:"max_digest_bytes"`
	MaxAttempts    int                `json:"max_attempts"`
	MaxDeliveryAge configkit.Duration `json:"max_delivery_age"`
	MinDispatchGap configkit.Duration `json:"min_dispatch_gap"`
	Destinations   map[string]string  `json:"destinations"`
	Fallback       map[string]string  `json:"fallback"`
}

func Default() Config {
	return Config{
		Timezone:       "UTC",
		QuietHours:     QuietHours{Enabled: true, Start: "23:00", End: "07:00"},
		MaxDigestItems: 20,
		MaxDigestBytes: 12000,
		MaxAttempts:    3,
		MaxDeliveryAge: configkit.Duration(24 * time.Hour),
		MinDispatchGap: configkit.Duration(5 * time.Second),
		Destinations:   map[string]string{},
		Fallback:       map[string]string{},
	}
}

func ValidRoute(route string) bool {
	_, _, err := ParseRoute(route)
	return err == nil
}

func (c *Config) Validate() error {
	if c == nil {
		return configkit.Invalid("", "must not be nil")
	}
	var problems []error
	if c.Timezone == "" {
		problems = append(problems, configkit.Invalid("timezone", "must not be empty"))
	} else if _, err := time.LoadLocation(c.Timezone); err != nil {
		problems = append(problems, configkit.Invalid("timezone", "is not a valid IANA timezone"))
	}
	if _, err := ParseHourMinute(c.QuietHours.Start); err != nil {
		problems = append(problems, configkit.Invalid("quiet_hours.start", "must be HH:MM"))
	}
	if _, err := ParseHourMinute(c.QuietHours.End); err != nil {
		problems = append(problems, configkit.Invalid("quiet_hours.end", "must be HH:MM"))
	}
	if c.MaxDigestItems <= 0 {
		problems = append(problems, configkit.Invalid("max_digest_items", "must be positive"))
	}
	if c.MaxDigestBytes <= 0 {
		problems = append(problems, configkit.Invalid("max_digest_bytes", "must be positive"))
	}
	if c.MaxAttempts <= 0 {
		problems = append(problems, configkit.Invalid("max_attempts", "must be positive"))
	}
	if c.MaxDeliveryAge <= 0 {
		problems = append(problems, configkit.Invalid("max_delivery_age", "must be positive"))
	}
	if c.MinDispatchGap < 0 {
		problems = append(problems, configkit.Invalid("min_dispatch_gap", "must not be negative"))
	}
	for alias, route := range c.Destinations {
		if !ValidAlias(alias) {
			problems = append(problems, configkit.Invalid("destinations", fmt.Sprintf("alias %q is not valid", alias)))
		}
		if !ValidRoute(route) {
			problems = append(problems, configkit.Invalid("destinations["+alias+"]", "route is not a channel destination"))
		}
	}
	for alias, route := range c.Fallback {
		if !ValidAlias(alias) {
			problems = append(problems, configkit.Invalid("fallback", fmt.Sprintf("alias %q is not valid", alias)))
		}
		if route == "" {
			continue
		}
		if !ValidRoute(route) {
			problems = append(problems, configkit.Invalid("fallback["+alias+"]", "route is not a channel destination"))
		}
	}
	return errors.Join(problems...)
}
