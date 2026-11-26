package webhook

import (
	"errors"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
)

func TestConfigDefaultIsValid(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default().Validate() = %v, want nil", err)
	}
	if cfg.Enabled {
		t.Error("default Enabled = true, want false")
	}
	if cfg.Listen != "127.0.0.1:8282" {
		t.Errorf("default Listen = %q, want 127.0.0.1:8282", cfg.Listen)
	}
	if cfg.MaxBodySize != configkit.ByteSize(1<<20) {
		t.Errorf("default MaxBodySize = %d, want %d", cfg.MaxBodySize, 1<<20)
	}
	if cfg.TimestampTolerance != configkit.Duration(5*time.Minute) {
		t.Errorf("default TimestampTolerance = %v, want 5m", cfg.TimestampTolerance)
	}
	if cfg.ReplayRetention != configkit.Duration(24*time.Hour) {
		t.Errorf("default ReplayRetention = %v, want 24h", cfg.ReplayRetention)
	}
	if cfg.RequestsPerMinute != 60 {
		t.Errorf("default RequestsPerMinute = %d, want 60", cfg.RequestsPerMinute)
	}
}

func TestConfigValidateBoundaries(t *testing.T) {
	t.Parallel()

	future := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)
	past := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)

	t.Run("nil config", func(t *testing.T) {
		t.Parallel()
		var cfg *Config
		if err := cfg.Validate(); !fault.HasCode(err, configkit.ErrorCodeInvalid) {
			t.Fatalf("nil Validate() = %v, want %s", err, configkit.ErrorCodeInvalid)
		}
	})

	t.Run("enabled with active and expired key", func(t *testing.T) {
		t.Parallel()
		cfg := Default()
		cfg.Enabled = true
		cfg.Keys = []Key{
			{ID: "old", SecretEnv: "AURA_OLD_KEY", AcceptUntil: past},
			{ID: "new", SecretEnv: "AURA_NEW_KEY", AcceptUntil: future},
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
	})

	cases := []struct {
		name     string
		mutate   func(*Config)
		wantPath string
	}{
		{
			name:     "bad listen address",
			mutate:   func(c *Config) { c.Listen = "no-port" },
			wantPath: "listen_address",
		},
		{
			name:     "zero max_body_size",
			mutate:   func(c *Config) { c.MaxBodySize = 0 },
			wantPath: "max_body_size",
		},
		{
			name:     "zero timestamp_tolerance",
			mutate:   func(c *Config) { c.TimestampTolerance = 0 },
			wantPath: "timestamp_tolerance",
		},
		{
			name:     "zero replay_retention",
			mutate:   func(c *Config) { c.ReplayRetention = 0 },
			wantPath: "replay_retention",
		},
		{
			name:     "negative replay_retention",
			mutate:   func(c *Config) { c.ReplayRetention = configkit.Duration(-time.Second) },
			wantPath: "replay_retention",
		},
		{
			name:     "zero requests_per_minute",
			mutate:   func(c *Config) { c.RequestsPerMinute = 0 },
			wantPath: "requests_per_minute",
		},
		{
			name:     "empty key id",
			mutate:   func(c *Config) { c.Keys = []Key{{ID: "", SecretEnv: "AURA_SECRET"}} },
			wantPath: "keys[0].id",
		},
		{
			name: "duplicate key id",
			mutate: func(c *Config) {
				c.Keys = []Key{
					{ID: "dup", SecretEnv: "AURA_A"},
					{ID: "dup", SecretEnv: "AURA_B"},
				}
			},
			wantPath: "keys[1].id",
		},
		{
			name:     "invalid secret_env",
			mutate:   func(c *Config) { c.Keys = []Key{{ID: "k", SecretEnv: "1BAD-ENV"}} },
			wantPath: "keys[0].secret_env",
		},
		{
			name:     "invalid accept_until",
			mutate:   func(c *Config) { c.Keys = []Key{{ID: "k", SecretEnv: "AURA_A", AcceptUntil: "not-a-date"}} },
			wantPath: "keys[0].accept_until",
		},
		{
			name:     "enabled without keys",
			mutate:   func(c *Config) { c.Enabled = true },
			wantPath: "enabled",
		},
		{
			name: "enabled with only expired keys",
			mutate: func(c *Config) {
				c.Enabled = true
				c.Keys = []Key{{ID: "old", SecretEnv: "AURA_OLD", AcceptUntil: past}}
			},
			wantPath: "enabled",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error on %s", tc.wantPath)
			}
			if !fault.HasCode(err, configkit.ErrorCodeInvalid) {
				t.Fatalf("Validate() = %v, want %s", err, configkit.ErrorCodeInvalid)
			}
			fieldErr, ok := errors.AsType[*configkit.FieldError](err)
			if !ok || fieldErr.Path != tc.wantPath {
				t.Fatalf("FieldError = %+v (ok=%v), want path %s", fieldErr, ok, tc.wantPath)
			}
		})
	}
}
