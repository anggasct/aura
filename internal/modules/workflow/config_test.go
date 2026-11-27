package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
)

func TestConfigDefault(t *testing.T) {
	cfg := Default()
	if cfg.MaxConcurrentSteps != 4 {
		t.Fatalf("MaxConcurrentSteps = %d, want 4", cfg.MaxConcurrentSteps)
	}
	if cfg.DefaultStepTimeout != configkit.Duration(15*time.Minute) {
		t.Fatalf("DefaultStepTimeout = %v, want 15m", cfg.DefaultStepTimeout)
	}
	if cfg.Generated != (GeneratedConfig{MaxSteps: 20, MaxTokens: 200000}) {
		t.Fatalf("Generated = %+v, want {20 200000}", cfg.Generated)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default().Validate() = %v, want nil", err)
	}
}

func TestConfigValidate(t *testing.T) {
	var nilCfg *Config
	if err := nilCfg.Validate(); err != nil {
		t.Fatalf("nil Config.Validate() = %v, want nil", err)
	}

	boundary := Config{
		MaxConcurrentSteps: 1,
		Generated:          GeneratedConfig{MaxSteps: 1, MaxTokens: 1},
	}
	if err := boundary.Validate(); err != nil {
		t.Fatalf("boundary Validate() = %v, want nil", err)
	}

	cases := []struct {
		name      string
		mutate    func(*Config)
		wantField string
	}{
		{
			name:      "zero max concurrent steps",
			mutate:    func(c *Config) { c.MaxConcurrentSteps = 0 },
			wantField: "max_concurrent_steps",
		},
		{
			name:      "negative max concurrent steps",
			mutate:    func(c *Config) { c.MaxConcurrentSteps = -2 },
			wantField: "max_concurrent_steps",
		},
		{
			name:      "zero generated max steps",
			mutate:    func(c *Config) { c.Generated.MaxSteps = 0 },
			wantField: "generated",
		},
		{
			name:      "negative generated max steps",
			mutate:    func(c *Config) { c.Generated.MaxSteps = -1 },
			wantField: "generated",
		},
		{
			name:      "zero generated max tokens",
			mutate:    func(c *Config) { c.Generated.MaxTokens = 0 },
			wantField: "generated",
		},
		{
			name:      "negative generated max tokens",
			mutate:    func(c *Config) { c.Generated.MaxTokens = -5 },
			wantField: "generated",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if !fault.HasCode(err, configkit.ErrorCodeInvalid) {
				t.Fatalf("Validate() = %v, want %s", err, configkit.ErrorCodeInvalid)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Fatalf("Validate() = %q, want field %q", err.Error(), tc.wantField)
			}
		})
	}

	both := Config{MaxConcurrentSteps: 0, Generated: GeneratedConfig{MaxSteps: 0, MaxTokens: 0}}
	err := both.Validate()
	if err == nil || !strings.Contains(err.Error(), "max_concurrent_steps") || !strings.Contains(err.Error(), "generated") {
		t.Fatalf("Validate() with both invalid = %v, want both fields reported", err)
	}
}
