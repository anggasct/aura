package schedule

import (
	"errors"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
)

type Config struct {
	Enabled             bool               `json:"enabled"`
	DefaultTimezone     string             `json:"default_timezone"`
	DefaultCatchUpGrace configkit.Duration `json:"default_catch_up_grace"`
	OccurrenceRetention configkit.Duration `json:"occurrence_retention"`
}

func Default() Config {
	return Config{
		Enabled:             true,
		DefaultTimezone:     "UTC",
		DefaultCatchUpGrace: configkit.Duration(15 * time.Minute),
		OccurrenceRetention: configkit.Duration(720 * time.Hour),
	}
}

func (c *Config) Validate() error {
	if c == nil {
		return configkit.Invalid("", "must not be nil")
	}
	var problems []error
	if c.DefaultTimezone == "" {
		problems = append(problems, configkit.Invalid("default_timezone", "must not be empty"))
	} else if _, err := time.LoadLocation(c.DefaultTimezone); err != nil {
		problems = append(problems, configkit.Invalid("default_timezone", "is not a valid IANA timezone"))
	}
	if c.DefaultCatchUpGrace < 0 {
		problems = append(problems, configkit.Invalid("default_catch_up_grace", "must not be negative"))
	}
	if c.OccurrenceRetention <= 0 {
		problems = append(problems, configkit.Invalid("occurrence_retention", "must be positive"))
	}
	return errors.Join(problems...)
}
