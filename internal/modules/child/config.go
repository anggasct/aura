package child

import (
	"errors"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
)

type Config struct {
	Enabled            bool               `json:"enabled"`
	MaxDepth           int                `json:"max_depth"`
	MaxActivePerParent int                `json:"max_active_per_parent"`
	MaxActiveGlobal    int                `json:"max_active_global"`
	DefaultTimeout     configkit.Duration `json:"default_timeout"`
	MaxTimeout         configkit.Duration `json:"max_timeout"`
	Recovery           string             `json:"recovery"`
}

func Default() *Config {
	return &Config{
		Enabled:            true,
		MaxDepth:           1,
		MaxActivePerParent: 2,
		MaxActiveGlobal:    4,
		DefaultTimeout:     configkit.Duration(5 * time.Minute),
		MaxTimeout:         configkit.Duration(30 * time.Minute),
		Recovery:           "interrupt",
	}
}

func (c *Config) Validate() error {
	if c == nil {
		return nil
	}
	var problems []error
	if c.MaxDepth != 1 {
		problems = append(problems, configkit.Invalid("max_depth", "must be 1"))
	}
	if c.MaxActivePerParent <= 0 || c.MaxActivePerParent > 64 {
		problems = append(problems, configkit.Invalid("max_active_per_parent", "must be between 1 and 64"))
	}
	if c.MaxActiveGlobal <= 0 || c.MaxActiveGlobal > 256 {
		problems = append(problems, configkit.Invalid("max_active_global", "must be between 1 and 256"))
	}
	if time.Duration(c.DefaultTimeout) <= 0 || time.Duration(c.DefaultTimeout) > 24*time.Hour {
		problems = append(problems, configkit.Invalid("default_timeout", "must be positive and at most 24h"))
	}
	if time.Duration(c.MaxTimeout) <= 0 || time.Duration(c.MaxTimeout) > 24*time.Hour {
		problems = append(problems, configkit.Invalid("max_timeout", "must be positive and at most 24h"))
	}
	if time.Duration(c.DefaultTimeout) > time.Duration(c.MaxTimeout) {
		problems = append(problems, configkit.Invalid("default_timeout", "must not exceed children.max_timeout"))
	}
	if c.Recovery != "interrupt" {
		problems = append(problems, configkit.Invalid("recovery", `must be "interrupt"`))
	}
	return errors.Join(problems...)
}
