package compaction

import (
	"errors"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/configkit"
)

func TestCompactionConfigValidate(t *testing.T) {
	t.Run("nil receiver", func(t *testing.T) {
		var cfg *Config
		if err := cfg.Validate(); err != nil {
			t.Fatalf("nil Validate() = %v, want nil", err)
		}
	})

	t.Run("default is valid", func(t *testing.T) {
		cfg := Default()
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Default().Validate() = %v", err)
		}
	})

	t.Run("accepted boundaries", func(t *testing.T) {
		cfg := Config{
			Enabled:                    false,
			RecentCompleteTurns:        1,
			SafetyMarginTokens:         1,
			ConservativeEstimatorRatio: 0.5,
			HighWaterRatio:             0.5,
			MaxToolExcerptBytes:        1,
			Summary: SummaryConfig{
				Task:            "compression",
				MaxSourceTokens: 1,
				MaxOutputTokens: 1,
				PromptVersion:   "v1",
			},
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() = %v", err)
		}
	})

	cases := map[string]struct {
		mutate   func(*Config)
		wantPath string
		wantText string
	}{
		"zero recent_complete_turns": {
			mutate:   func(c *Config) { c.RecentCompleteTurns = 0 },
			wantPath: "recent_complete_turns",
		},
		"negative recent_complete_turns": {
			mutate:   func(c *Config) { c.RecentCompleteTurns = -1 },
			wantPath: "recent_complete_turns",
		},
		"zero safety_margin_tokens": {
			mutate:   func(c *Config) { c.SafetyMarginTokens = 0 },
			wantPath: "safety_margin_tokens",
		},
		"conservative ratio zero": {
			mutate:   func(c *Config) { c.ConservativeEstimatorRatio = 0 },
			wantPath: "conservative_estimator_ratio",
		},
		"conservative ratio one": {
			mutate:   func(c *Config) { c.ConservativeEstimatorRatio = 1 },
			wantPath: "conservative_estimator_ratio",
		},
		"high water ratio zero": {
			mutate:   func(c *Config) { c.HighWaterRatio = 0 },
			wantPath: "high_water_ratio",
		},
		"high water ratio one": {
			mutate:   func(c *Config) { c.HighWaterRatio = 1 },
			wantPath: "high_water_ratio",
		},
		"conservative exceeds high water": {
			mutate: func(c *Config) {
				c.ConservativeEstimatorRatio = 0.95
				c.HighWaterRatio = 0.90
			},
			wantPath: "conservative_estimator_ratio",
			wantText: "must not exceed context.high_water_ratio",
		},
		"zero max_tool_excerpt_bytes": {
			mutate:   func(c *Config) { c.MaxToolExcerptBytes = 0 },
			wantPath: "max_tool_excerpt_bytes",
		},
		"empty summary task": {
			mutate:   func(c *Config) { c.Summary.Task = "   " },
			wantPath: "summary.task",
		},
		"zero summary max_source_tokens": {
			mutate:   func(c *Config) { c.Summary.MaxSourceTokens = 0 },
			wantPath: "summary.max_source_tokens",
		},
		"zero summary max_output_tokens": {
			mutate:   func(c *Config) { c.Summary.MaxOutputTokens = 0 },
			wantPath: "summary.max_output_tokens",
		},
		"empty summary prompt_version": {
			mutate:   func(c *Config) { c.Summary.PromptVersion = "\t" },
			wantPath: "summary.prompt_version",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error for %s", name)
			}
			var fe *configkit.FieldError
			if !errors.As(err, &fe) {
				t.Fatalf("expected FieldError, got %T (%v)", err, err)
			}
			if fe.Path != tc.wantPath {
				t.Errorf("path = %q, want %q (err=%v)", fe.Path, tc.wantPath, err)
			}
			if tc.wantText != "" && !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("err = %q, want substring %q", err.Error(), tc.wantText)
			}
		})
	}

	t.Run("joins multiple errors", func(t *testing.T) {
		cfg := Config{
			ConservativeEstimatorRatio: 0.95,
			HighWaterRatio:             0.90,
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected joined errors")
		}
		for _, path := range []string{
			"recent_complete_turns",
			"safety_margin_tokens",
			"conservative_estimator_ratio",
			"max_tool_excerpt_bytes",
			"summary.task",
			"summary.max_source_tokens",
			"summary.max_output_tokens",
			"summary.prompt_version",
		} {
			if !strings.Contains(err.Error(), path) {
				t.Errorf("joined error missing %q: %v", path, err)
			}
		}
	})
}
