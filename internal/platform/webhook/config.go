package webhook

import (
	"errors"
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/secret"
)

type Config struct {
	Enabled            bool               `json:"enabled"`
	Listen             string             `json:"listen_address"`
	MaxBodySize        configkit.ByteSize `json:"max_body_size"`
	TimestampTolerance configkit.Duration `json:"timestamp_tolerance"`
	ReplayRetention    configkit.Duration `json:"replay_retention"`
	RequestsPerMinute  int                `json:"requests_per_minute"`
	Keys               []Key              `json:"keys"`
}

type Key struct {
	ID          string `json:"id"`
	SecretEnv   string `json:"secret_env"`
	AcceptUntil string `json:"accept_until"`
}

func Default() Config {
	return Config{
		Listen:             "127.0.0.1:8282",
		MaxBodySize:        configkit.ByteSize(1 << 20),
		TimestampTolerance: configkit.Duration(5 * time.Minute),
		ReplayRetention:    configkit.Duration(24 * time.Hour),
		RequestsPerMinute:  60,
	}
}

func (c *Config) Validate() error {
	if c == nil {
		return configkit.Invalid("", "must not be nil")
	}
	var problems []error
	if err := configkit.ValidateListenAddress("listen_address", c.Listen); err != nil {
		problems = append(problems, err)
	}
	if c.MaxBodySize <= 0 {
		problems = append(problems, configkit.Invalid("max_body_size", "must be positive"))
	}
	if c.TimestampTolerance <= 0 {
		problems = append(problems, configkit.Invalid("timestamp_tolerance", "must be positive"))
	}
	if c.ReplayRetention <= 0 {
		problems = append(problems, configkit.Invalid("replay_retention", "must be positive"))
	}
	if c.RequestsPerMinute < 1 {
		problems = append(problems, configkit.Invalid("requests_per_minute", "must be at least 1"))
	}
	seen := make(map[string]bool, len(c.Keys))
	keyProblems := false
	for i, key := range c.Keys {
		field := fmt.Sprintf("keys[%d]", i)
		switch {
		case key.ID == "":
			problems = append(problems, configkit.Invalid(field+".id", "must not be empty"))
			keyProblems = true
			continue
		case seen[key.ID]:
			problems = append(problems, configkit.Invalid(field+".id", fmt.Sprintf("%q is configured more than once", key.ID)))
			keyProblems = true
			continue
		}
		seen[key.ID] = true
		if !secret.ValidEnvName(key.SecretEnv) {
			problems = append(problems, configkit.Invalid(field+".secret_env", fmt.Sprintf("%q is not a valid environment variable name", key.SecretEnv)))
			keyProblems = true
		}
		if key.AcceptUntil != "" {
			if _, err := time.Parse(time.RFC3339, key.AcceptUntil); err != nil {
				problems = append(problems, configkit.Invalid(field+".accept_until", "must be an RFC3339 timestamp or empty"))
				keyProblems = true
			}
		}
	}
	if c.Enabled && !keyProblems {
		if len(c.Keys) == 0 {
			problems = append(problems, configkit.Invalid("enabled", "requires at least one key"))
		} else if !hasActiveKey(c.Keys, time.Now().UTC()) {
			problems = append(problems, configkit.Invalid("enabled", "requires at least one key whose accept_until has not passed"))
		}
	}
	return errors.Join(problems...)
}

func hasActiveKey(keys []Key, now time.Time) bool {
	for _, key := range keys {
		if key.AcceptUntil == "" {
			return true
		}
		until, err := time.Parse(time.RFC3339, key.AcceptUntil)
		if err == nil && until.After(now) {
			return true
		}
	}
	return false
}
