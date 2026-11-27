package runtimeengine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
)

func TestRuntimeConfigValidate(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*Config)
		wantPath string
		wantText string
	}{
		{"active zero", func(c *Config) { c.MaxActiveTurns = 0 }, "max_active_turns", "must be positive"},
		{"active negative", func(c *Config) { c.MaxActiveTurns = -1 }, "max_active_turns", "must be positive"},
		{"pending one below active", func(c *Config) { c.MaxActiveTurns, c.MaxPendingTurns = 8, 7 }, "max_pending_turns", "must be at least runtime.max_active_turns"},
		{"turn timeout zero", func(c *Config) { c.TurnTimeout = 0 }, "turn_timeout", "must be positive"},
		{"turn timeout negative", func(c *Config) { c.TurnTimeout = -1 }, "turn_timeout", "must be positive"},
		{"answer reserve zero", func(c *Config) { c.AnswerReserve = 0 }, "answer_reserve", "must be positive"},
		{"answer reserve negative", func(c *Config) { c.AnswerReserve = configkit.Duration(-time.Second) }, "answer_reserve", "must be positive"},
		{"answer reserve equals turn timeout", func(c *Config) { c.AnswerReserve = c.TurnTimeout }, "answer_reserve", "must be less than runtime.turn_timeout"},
		{"answer reserve exceeds turn timeout", func(c *Config) { c.AnswerReserve = c.TurnTimeout + configkit.Duration(time.Second) }, "answer_reserve", "must be less than runtime.turn_timeout"},
		{"consecutive errors zero", func(c *Config) { c.MaxConsecutiveToolErrors = 0 }, "max_consecutive_tool_errors", "must be at least 1"},
		{"consecutive errors negative", func(c *Config) { c.MaxConsecutiveToolErrors = -1 }, "max_consecutive_tool_errors", "must be at least 1"},
		{"total errors below consecutive", func(c *Config) { c.MaxConsecutiveToolErrors, c.MaxToolErrorsPerTurn = 4, 3 }, "max_tool_errors_per_turn", "must be at least runtime.max_consecutive_tool_errors"},
		{"shutdown timeout zero", func(c *Config) { c.ShutdownTimeout = 0 }, "shutdown_timeout", "must be positive"},
		{"retention one second below the floor", func(c *Config) { c.RunRetention = configkit.Duration(MinRunRetention - time.Second) }, "run_retention", "must be at least 24h"},
		{"first problem wins", func(c *Config) { c.MaxActiveTurns, c.RunRetention = 0, 0 }, "max_active_turns", "must be positive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if !fault.HasCode(err, configkit.ErrorCodeInvalid) {
				t.Fatalf("Validate err = %v, want code %q", err, configkit.ErrorCodeInvalid)
			}
			field, _ := errors.AsType[*configkit.FieldError](err)
			if field == nil || field.Path != tc.wantPath {
				t.Fatalf("Validate err = %v, want field path %q", err, tc.wantPath)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("Validate err = %q, want text %q", err, tc.wantText)
			}
		})
	}
}

func TestRuntimeConfigValidateAcceptsTheBoundaries(t *testing.T) {
	defaults := Default()
	if time.Duration(defaults.AnswerReserve) != 30*time.Second || defaults.MaxConsecutiveToolErrors != 3 || defaults.MaxToolErrorsPerTurn != 8 {
		t.Fatalf("defaults = %+v, want answer_reserve=30s max_consecutive_tool_errors=3 max_tool_errors_per_turn=8", defaults)
	}
	cases := map[string]func(*Config){
		"defaults":                          func(*Config) {},
		"pending equals active":             func(c *Config) { c.MaxActiveTurns, c.MaxPendingTurns = 3, 3 },
		"answer reserve just below timeout": func(c *Config) { c.AnswerReserve = c.TurnTimeout - 1 },
		"consecutive errors at floor":       func(c *Config) { c.MaxConsecutiveToolErrors, c.MaxToolErrorsPerTurn = 1, 1 },
		"total errors equals consecutive":   func(c *Config) { c.MaxConsecutiveToolErrors, c.MaxToolErrorsPerTurn = 5, 5 },
		"retention exactly the floor":       func(c *Config) { c.RunRetention = configkit.Duration(MinRunRetention) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			mutate(&cfg)
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate err = %v, want nil", err)
			}
		})
	}
}
