package workflow

import (
	"errors"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
)

type Config struct {
	DefinitionsDir     string             `json:"definitions_dir"`
	MaxConcurrentSteps int                `json:"max_concurrent_steps"`
	DefaultStepTimeout configkit.Duration `json:"default_step_timeout"`
	Generated          GeneratedConfig    `json:"generated"`
}

type GeneratedConfig struct {
	MaxSteps  int   `json:"max_steps"`
	MaxTokens int64 `json:"max_tokens"`
}

func Default() Config {
	return Config{
		MaxConcurrentSteps: 4,
		DefaultStepTimeout: configkit.Duration(15 * time.Minute),
		Generated:          GeneratedConfig{MaxSteps: 20, MaxTokens: 200000},
	}
}

func (c *Config) Validate() error {
	if c == nil {
		return nil
	}
	var problems []error
	if c.MaxConcurrentSteps < 1 {
		problems = append(problems, configkit.Invalid("max_concurrent_steps", "must be at least 1"))
	}
	if c.Generated.MaxSteps < 1 || c.Generated.MaxTokens < 1 {
		problems = append(problems, configkit.Invalid("generated", "max_steps and max_tokens must be positive"))
	}
	return errors.Join(problems...)
}
