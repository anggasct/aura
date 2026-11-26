package memory

import (
	"errors"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
)

type Config struct {
	Enabled              bool               `json:"enabled"`
	MaxDocuments         int                `json:"max_documents"`
	RecallTokenBudget    int                `json:"recall_token_budget"`
	Locale               string             `json:"locale"`
	EnglishStemming      bool               `json:"english_stemming"`
	SummaryPromptVersion string             `json:"summary_prompt_version"`
	SummaryTTL           configkit.Duration `json:"summary_ttl"`
	RangeThreshold       int                `json:"range_threshold"`
	RangeLiveWindow      int                `json:"range_live_window"`
	PurgeInterval        configkit.Duration `json:"purge_interval"`
	PurgeLimit           int                `json:"purge_limit"`
}

func Default() Config {
	return Config{
		Enabled:              true,
		MaxDocuments:         10,
		RecallTokenBudget:    2000,
		SummaryPromptVersion: "memory-summary-v1",
		SummaryTTL:           configkit.Duration(720 * time.Hour),
		RangeThreshold:       20,
		RangeLiveWindow:      5,
		PurgeInterval:        configkit.Duration(1 * time.Hour),
		PurgeLimit:           100,
	}
}

func (c *Config) Validate() error {
	if c == nil {
		return configkit.Invalid("", "must not be nil")
	}
	var problems []error
	if c.MaxDocuments <= 0 || c.MaxDocuments > 50 {
		problems = append(problems, configkit.Invalid("max_documents", "must be between 1 and 50"))
	}
	if c.RecallTokenBudget <= 0 || c.RecallTokenBudget > 8000 {
		problems = append(problems, configkit.Invalid("recall_token_budget", "must be between 1 and 8000"))
	}
	if strings.TrimSpace(c.SummaryPromptVersion) == "" {
		problems = append(problems, configkit.Invalid("summary_prompt_version", "must not be empty"))
	}
	if c.SummaryTTL <= 0 {
		problems = append(problems, configkit.Invalid("summary_ttl", "must be positive"))
	}
	if c.EnglishStemming {
		problems = append(problems, configkit.Invalid("english_stemming", "is not supported by this build"))
	}
	if strings.TrimSpace(c.Locale) != "" {
		problems = append(problems, configkit.Invalid("locale", "is not supported by this build"))
	}
	return errors.Join(problems...)
}
