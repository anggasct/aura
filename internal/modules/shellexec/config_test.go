package shellexec

import (
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
)

func TestConfigDefaultIsValid(t *testing.T) {
	cfg := Default()
	if time.Duration(cfg.Timeout) != 30*time.Second || cfg.MaxStdoutBytes != 65536 || cfg.MaxStderrBytes != 65536 {
		t.Fatalf("Default() = %+v, want 30s / 65536 / 65536", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default().Validate() = %v, want nil", err)
	}
	var nilCfg *Config
	if err := nilCfg.Validate(); err != nil {
		t.Fatalf("(*Config)(nil).Validate() = %v, want nil", err)
	}
}

func TestConfigValidateRejectsInvalidBounds(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*Config)
		wantPath  string
		wantError string
	}{
		{
			name:      "zero timeout",
			mutate:    func(c *Config) { c.Timeout = 0 },
			wantPath:  "tools.exec.timeout",
			wantError: "must be positive",
		},
		{
			name:      "negative timeout",
			mutate:    func(c *Config) { c.Timeout = configkit.Duration(-time.Second) },
			wantPath:  "tools.exec.timeout",
			wantError: "must be positive",
		},
		{
			name:      "zero stdout limit",
			mutate:    func(c *Config) { c.MaxStdoutBytes = 0 },
			wantPath:  "tools.exec",
			wantError: "output limits must be positive",
		},
		{
			name:      "negative stdout limit",
			mutate:    func(c *Config) { c.MaxStdoutBytes = -1 },
			wantPath:  "tools.exec",
			wantError: "output limits must be positive",
		},
		{
			name:      "zero stderr limit",
			mutate:    func(c *Config) { c.MaxStderrBytes = 0 },
			wantPath:  "tools.exec",
			wantError: "output limits must be positive",
		},
		{
			name:      "negative stderr limit",
			mutate:    func(c *Config) { c.MaxStderrBytes = -1 },
			wantPath:  "tools.exec",
			wantError: "output limits must be positive",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := configkit.Under("tools.exec", cfg.Validate())
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if !fault.HasCode(err, configkit.ErrorCodeInvalid) {
				t.Fatalf("Validate err = %v, want code %q", err, configkit.ErrorCodeInvalid)
			}
			text := err.Error()
			if !strings.Contains(text, tc.wantPath) || !strings.Contains(text, tc.wantError) {
				t.Fatalf("Validate err = %q, want path %q and detail %q", text, tc.wantPath, tc.wantError)
			}
		})
	}
}

func TestConfigValidateJoinsMultipleErrors(t *testing.T) {
	cfg := Config{}
	err := configkit.Under("tools.exec", cfg.Validate())
	if err == nil {
		t.Fatal("Validate accepted zero config")
	}
	text := err.Error()
	if !strings.Contains(text, "tools.exec.timeout") || !strings.Contains(text, "output limits must be positive") {
		t.Fatalf("Validate err = %q, want both timeout and output limit errors", text)
	}
}
