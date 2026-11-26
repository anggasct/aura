package memory

import (
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
)

func TestMemoryConfigValidate(t *testing.T) {
	t.Run("nil config", func(t *testing.T) {
		var cfg *Config
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "must not be nil") {
			t.Fatalf("Validate(nil) = %v, want non-nil error", err)
		}
	})

	t.Run("defaults valid", func(t *testing.T) {
		cfg := Default()
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Default().Validate() = %v", err)
		}
	})

	t.Run("accepted boundaries", func(t *testing.T) {
		cases := []struct {
			name   string
			mutate func(*Config)
		}{
			{"min documents", func(c *Config) { c.MaxDocuments = 1 }},
			{"max documents", func(c *Config) { c.MaxDocuments = 50 }},
			{"min budget", func(c *Config) { c.RecallTokenBudget = 1 }},
			{"max budget", func(c *Config) { c.RecallTokenBudget = 8000 }},
			{"positive ttl", func(c *Config) { c.SummaryTTL = configkit.Duration(time.Second) }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				cfg := Default()
				tc.mutate(&cfg)
				if err := cfg.Validate(); err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
			})
		}
	})

	t.Run("rejected boundaries", func(t *testing.T) {
		cases := []struct {
			name   string
			mutate func(*Config)
			want   string
		}{
			{"zero documents", func(c *Config) { c.MaxDocuments = 0 }, "max_documents"},
			{"too many documents", func(c *Config) { c.MaxDocuments = 51 }, "max_documents"},
			{"zero budget", func(c *Config) { c.RecallTokenBudget = 0 }, "recall_token_budget"},
			{"too large budget", func(c *Config) { c.RecallTokenBudget = 8001 }, "recall_token_budget"},
			{"blank prompt version", func(c *Config) { c.SummaryPromptVersion = "   " }, "summary_prompt_version"},
			{"non-positive ttl", func(c *Config) { c.SummaryTTL = 0 }, "summary_ttl"},
			{"english stemming", func(c *Config) { c.EnglishStemming = true }, "english_stemming"},
			{"locale", func(c *Config) { c.Locale = "en-US" }, "locale"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				cfg := Default()
				tc.mutate(&cfg)
				err := cfg.Validate()
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("Validate() = %v, want %q", err, tc.want)
				}
			})
		}
	})

	t.Run("joins multiple failures", func(t *testing.T) {
		cfg := Config{
			MaxDocuments:         0,
			RecallTokenBudget:    8001,
			SummaryPromptVersion: "",
			SummaryTTL:           0,
			EnglishStemming:      true,
			Locale:               "en",
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("Validate() = nil, want joined errors")
		}
		for _, field := range []string{
			"max_documents",
			"recall_token_budget",
			"summary_prompt_version",
			"summary_ttl",
			"english_stemming",
			"locale",
		} {
			if !strings.Contains(err.Error(), field) {
				t.Errorf("joined error %q missing %q", err.Error(), field)
			}
		}
	})
}
