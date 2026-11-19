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
	cases := map[string]func(*Config){
		"defaults":                    func(*Config) {},
		"pending equals active":       func(c *Config) { c.MaxActiveTurns, c.MaxPendingTurns = 3, 3 },
		"retention exactly the floor": func(c *Config) { c.RunRetention = configkit.Duration(MinRunRetention) },
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
