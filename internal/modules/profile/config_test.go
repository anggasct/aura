package profile

import (
	"errors"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/configkit"
)

func TestProfileConfigValidate(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("Default().Validate() = %v, want nil", err)
	}

	valid := map[string]func(*Config){
		"min boundaries": func(c *Config) {
			c.MaxContextFacts = 1
			c.MaxContextTokens = 1
			c.ExtractionQueueCapacity = 1
			c.CandidateMinConfidence = 0
		},
		"max boundaries": func(c *Config) {
			c.MaxContextFacts = 1000
			c.MaxContextTokens = 128000
			c.ExtractionQueueCapacity = 10000
			c.CandidateMinConfidence = 1
		},
	}
	for name, mutate := range valid {
		t.Run("accept/"+name, func(t *testing.T) {
			cfg := Default()
			mutate(&cfg)
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}

	invalid := map[string]struct {
		mutate func(*Config)
		path   string
	}{
		"facts zero":           {func(c *Config) { c.MaxContextFacts = 0 }, "max_context_facts"},
		"facts too high":       {func(c *Config) { c.MaxContextFacts = 1001 }, "max_context_facts"},
		"tokens zero":          {func(c *Config) { c.MaxContextTokens = 0 }, "max_context_tokens"},
		"tokens too high":      {func(c *Config) { c.MaxContextTokens = 128001 }, "max_context_tokens"},
		"queue zero":           {func(c *Config) { c.ExtractionQueueCapacity = 0 }, "extraction_queue_capacity"},
		"queue too high":       {func(c *Config) { c.ExtractionQueueCapacity = 10001 }, "extraction_queue_capacity"},
		"confidence negative":  {func(c *Config) { c.CandidateMinConfidence = -0.01 }, "candidate_min_confidence"},
		"confidence above one": {func(c *Config) { c.CandidateMinConfidence = 1.01 }, "candidate_min_confidence"},
		"empty prompt version": {func(c *Config) { c.PromptVersion = "  " }, "prompt_version"},
	}
	for name, tc := range invalid {
		t.Run("reject/"+name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error for %s", tc.path)
			}
			var fieldErr *configkit.FieldError
			if !errors.As(err, &fieldErr) || fieldErr.Path != tc.path {
				t.Fatalf("err = %v, want field path %q", err, tc.path)
			}
		})
	}

	t.Run("joins multiple failures", func(t *testing.T) {
		cfg := Config{
			MaxContextFacts:         0,
			MaxContextTokens:        0,
			ExtractionQueueCapacity: 0,
			CandidateMinConfidence:  2,
			PromptVersion:           "",
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected joined validation error")
		}
		text := err.Error()
		for _, want := range []string{
			"max_context_facts",
			"max_context_tokens",
			"extraction_queue_capacity",
			"candidate_min_confidence",
			"prompt_version",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("joined error missing %q: %v", want, err)
			}
		}
	})
}
