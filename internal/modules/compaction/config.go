package compaction

import (
	"errors"
	"strings"

	"github.com/anggasct/aura/internal/platform/configkit"
)

type Config struct {
	Enabled                    bool          `json:"enabled"`
	RecentCompleteTurns        int           `json:"recent_complete_turns"`
	SafetyMarginTokens         int           `json:"safety_margin_tokens"`
	ConservativeEstimatorRatio float64       `json:"conservative_estimator_ratio"`
	HighWaterRatio             float64       `json:"high_water_ratio"`
	MaxToolExcerptBytes        int           `json:"max_tool_excerpt_bytes"`
	Summary                    SummaryConfig `json:"summary"`
}

type SummaryConfig struct {
	Task            string `json:"task"`
	MaxSourceTokens int    `json:"max_source_tokens"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	PromptVersion   string `json:"prompt_version"`
}

func Default() Config {
	return Config{
		Enabled:                    true,
		RecentCompleteTurns:        10,
		SafetyMarginTokens:         1024,
		ConservativeEstimatorRatio: 0.80,
		HighWaterRatio:             0.90,
		MaxToolExcerptBytes:        2048,
		Summary: SummaryConfig{
			Task:            "compression",
			MaxSourceTokens: 32768,
			MaxOutputTokens: 2048,
			PromptVersion:   "v1",
		},
	}
}

func (c *Config) Validate() error {
	if c == nil {
		return nil
	}
	var problems []error
	if c.RecentCompleteTurns <= 0 {
		problems = append(problems, configkit.Invalid("recent_complete_turns", "must be positive"))
	}
	if c.SafetyMarginTokens <= 0 {
		problems = append(problems, configkit.Invalid("safety_margin_tokens", "must be positive"))
	}
	if c.ConservativeEstimatorRatio <= 0 || c.ConservativeEstimatorRatio >= 1 {
		problems = append(problems, configkit.Invalid("conservative_estimator_ratio", "must be in (0,1)"))
	}
	if c.HighWaterRatio <= 0 || c.HighWaterRatio >= 1 {
		problems = append(problems, configkit.Invalid("high_water_ratio", "must be in (0,1)"))
	}
	if c.ConservativeEstimatorRatio > c.HighWaterRatio {
		problems = append(problems, configkit.Invalid("conservative_estimator_ratio", "must not exceed context.high_water_ratio"))
	}
	if c.MaxToolExcerptBytes <= 0 {
		problems = append(problems, configkit.Invalid("max_tool_excerpt_bytes", "must be positive"))
	}
	if strings.TrimSpace(c.Summary.Task) == "" {
		problems = append(problems, configkit.Invalid("summary.task", "must not be empty"))
	}
	if c.Summary.MaxSourceTokens <= 0 {
		problems = append(problems, configkit.Invalid("summary.max_source_tokens", "must be positive"))
	}
	if c.Summary.MaxOutputTokens <= 0 {
		problems = append(problems, configkit.Invalid("summary.max_output_tokens", "must be positive"))
	}
	if strings.TrimSpace(c.Summary.PromptVersion) == "" {
		problems = append(problems, configkit.Invalid("summary.prompt_version", "must not be empty"))
	}
	return errors.Join(problems...)
}
