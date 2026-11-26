package terminal

import (
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
)

func TestTerminalConfigValidate(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("Default().Validate() = %v, want nil", err)
	}

	zeroHistory := Default()
	zeroHistory.InMemoryHistory = 0
	if err := zeroHistory.Validate(); err != nil {
		t.Fatalf("zero in_memory_history Validate() = %v, want nil", err)
	}

	cases := []struct {
		name     string
		mutate   func(*Config)
		wantPath string
		wantMsg  string
	}{
		{
			name:     "non-positive render_hz",
			mutate:   func(c *Config) { c.RenderHz = 0 },
			wantPath: "render_hz",
			wantMsg:  "must be positive",
		},
		{
			name:     "non-positive max_input_bytes",
			mutate:   func(c *Config) { c.MaxInputBytes = 0 },
			wantPath: "max_input_bytes",
			wantMsg:  "must be positive",
		},
		{
			name:     "negative in_memory_history",
			mutate:   func(c *Config) { c.InMemoryHistory = -1 },
			wantPath: "in_memory_history",
			wantMsg:  "must not be negative",
		},
		{
			name:     "non-positive second_interrupt_window",
			mutate:   func(c *Config) { c.SecondInterruptTime = configkit.Duration(0) },
			wantPath: "second_interrupt_window",
			wantMsg:  "must be positive",
		},
		{
			name:     "allow plain_approval rejected",
			mutate:   func(c *Config) { c.PlainApproval = "allow" },
			wantPath: "plain_approval",
			wantMsg:  `must be "deny"`,
		},
		{
			name:     "empty plain_approval rejected",
			mutate:   func(c *Config) { c.PlainApproval = "" },
			wantPath: "plain_approval",
			wantMsg:  `must be "deny"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error for %s", tc.wantPath)
			}
			if !strings.Contains(err.Error(), tc.wantPath) || !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("Validate() = %q, want path %q and detail %q", err.Error(), tc.wantPath, tc.wantMsg)
			}
		})
	}

	allBad := Config{
		RenderHz:            0,
		MaxInputBytes:       -1,
		InMemoryHistory:     -5,
		SecondInterruptTime: configkit.Duration(-time.Second),
		PlainApproval:       "allow",
	}
	err := allBad.Validate()
	if err == nil {
		t.Fatal("Validate() on all-invalid config = nil, want joined errors")
	}
	for _, path := range []string{"render_hz", "max_input_bytes", "in_memory_history", "second_interrupt_window", "plain_approval"} {
		if !strings.Contains(err.Error(), path) {
			t.Errorf("joined error %q missing %q", err.Error(), path)
		}
	}
}
