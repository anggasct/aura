package child

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
)

func TestConfigDefaultIsValid(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default().Validate(): %v", err)
	}
	var nilCfg *Config
	if err := nilCfg.Validate(); err != nil {
		t.Fatalf("nil Validate(): %v", err)
	}
}

func TestConfigValidateBounds(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*Config)
		wantPath string
		wantText string
	}{
		{
			name:     "max_depth zero",
			mutate:   func(c *Config) { c.MaxDepth = 0 },
			wantPath: "max_depth",
			wantText: "must be 1",
		},
		{
			name:     "max_depth above one",
			mutate:   func(c *Config) { c.MaxDepth = 2 },
			wantPath: "max_depth",
			wantText: "must be 1",
		},
		{
			name:     "max_active_per_parent zero",
			mutate:   func(c *Config) { c.MaxActivePerParent = 0 },
			wantPath: "max_active_per_parent",
			wantText: "must be between 1 and 64",
		},
		{
			name:     "max_active_per_parent above limit",
			mutate:   func(c *Config) { c.MaxActivePerParent = 65 },
			wantPath: "max_active_per_parent",
			wantText: "must be between 1 and 64",
		},
		{
			name:     "max_active_global zero",
			mutate:   func(c *Config) { c.MaxActiveGlobal = 0 },
			wantPath: "max_active_global",
			wantText: "must be between 1 and 256",
		},
		{
			name:     "max_active_global above limit",
			mutate:   func(c *Config) { c.MaxActiveGlobal = 257 },
			wantPath: "max_active_global",
			wantText: "must be between 1 and 256",
		},
		{
			name:     "default_timeout zero",
			mutate:   func(c *Config) { c.DefaultTimeout = 0 },
			wantPath: "default_timeout",
			wantText: "must be positive and at most 24h",
		},
		{
			name:     "default_timeout above 24h",
			mutate:   func(c *Config) { c.DefaultTimeout = configkit.Duration(25 * time.Hour) },
			wantPath: "default_timeout",
			wantText: "must be positive and at most 24h",
		},
		{
			name:     "max_timeout zero",
			mutate:   func(c *Config) { c.MaxTimeout = 0 },
			wantPath: "max_timeout",
			wantText: "must be positive and at most 24h",
		},
		{
			name:     "max_timeout above 24h",
			mutate:   func(c *Config) { c.MaxTimeout = configkit.Duration(25 * time.Hour) },
			wantPath: "max_timeout",
			wantText: "must be positive and at most 24h",
		},
		{
			name: "default_timeout exceeds max_timeout",
			mutate: func(c *Config) {
				c.DefaultTimeout = configkit.Duration(10 * time.Minute)
				c.MaxTimeout = configkit.Duration(5 * time.Minute)
			},
			wantPath: "default_timeout",
			wantText: "must not exceed children.max_timeout",
		},
		{
			name:     "recovery unsupported",
			mutate:   func(c *Config) { c.Recovery = "resume" },
			wantPath: "recovery",
			wantText: `must be "interrupt"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			var fieldErr *configkit.FieldError
			if !errors.As(err, &fieldErr) {
				t.Fatalf("err = %v, want FieldError", err)
			}
			if fieldErr.Path != tc.wantPath || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("err = %v (path=%q), want path %q containing %q", err, fieldErr.Path, tc.wantPath, tc.wantText)
			}
		})
	}
}
