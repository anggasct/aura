package profile

import (
	"errors"
	"strings"

	"github.com/anggasct/aura/internal/platform/configkit"
)

type Config struct {
	Enabled                 bool    `json:"enabled"`
	MaxContextFacts         int     `json:"max_context_facts"`
	MaxContextTokens        int     `json:"max_context_tokens"`
	ExtractionQueueCapacity int     `json:"extraction_queue_capacity"`
	CandidateMinConfidence  float64 `json:"candidate_min_confidence"`
	PromptVersion           string  `json:"prompt_version"`
}

func Default() Config {
	return Config{
		Enabled:                 true,
		MaxContextFacts:         20,
		MaxContextTokens:        1024,
		ExtractionQueueCapacity: 64,
		CandidateMinConfidence:  0.70,
		PromptVersion:           "v1",
	}
}

func (c Config) Validate() error {
	var problems []error
	if c.MaxContextFacts <= 0 || c.MaxContextFacts > 1000 {
		problems = append(problems, configkit.Invalid("max_context_facts", "must be between 1 and 1000"))
	}
	if c.MaxContextTokens <= 0 || c.MaxContextTokens > 128000 {
		problems = append(problems, configkit.Invalid("max_context_tokens", "must be between 1 and 128000"))
	}
	if c.ExtractionQueueCapacity <= 0 || c.ExtractionQueueCapacity > 10000 {
		problems = append(problems, configkit.Invalid("extraction_queue_capacity", "must be between 1 and 10000"))
	}
	if c.CandidateMinConfidence < 0 || c.CandidateMinConfidence > 1 {
		problems = append(problems, configkit.Invalid("candidate_min_confidence", "must be in [0,1]"))
	}
	if strings.TrimSpace(c.PromptVersion) == "" {
		problems = append(problems, configkit.Invalid("prompt_version", "must not be empty"))
	}
	return errors.Join(problems...)
}
