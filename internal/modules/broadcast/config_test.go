package broadcast

import (
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
)

func TestBroadcastConfigDefaultValidates(t *testing.T) {
	cfg := Default()
	if cfg.Timezone != "UTC" || !cfg.QuietHours.Enabled || cfg.QuietHours.Start != "23:00" || cfg.QuietHours.End != "07:00" ||
		cfg.MaxDigestItems != 20 || cfg.MaxDigestBytes != 12000 || cfg.MaxAttempts != 3 ||
		cfg.MaxDeliveryAge != configkit.Duration(24*time.Hour) || cfg.MinDispatchGap != configkit.Duration(5*time.Second) {
		t.Fatalf("Default() = %+v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default().Validate(): %v", err)
	}
}

func TestBroadcastConfigValidate(t *testing.T) {
	t.Run("nil receiver", func(t *testing.T) {
		var cfg *Config
		if err := cfg.Validate(); err == nil {
			t.Fatal("nil config accepted")
		}
	})

	cases := []struct {
		name     string
		mutate   func(*Config)
		wantPath string
	}{
		{"empty timezone", func(c *Config) { c.Timezone = "" }, "timezone"},
		{"invalid timezone", func(c *Config) { c.Timezone = "Mars/Olympus" }, "timezone"},
		{"bad quiet start", func(c *Config) { c.QuietHours.Start = "25:00" }, "quiet_hours.start"},
		{"bad quiet end", func(c *Config) { c.QuietHours.End = "7am" }, "quiet_hours.end"},
		{"zero digest items", func(c *Config) { c.MaxDigestItems = 0 }, "max_digest_items"},
		{"zero digest bytes", func(c *Config) { c.MaxDigestBytes = 0 }, "max_digest_bytes"},
		{"zero attempts", func(c *Config) { c.MaxAttempts = 0 }, "max_attempts"},
		{"zero delivery age", func(c *Config) { c.MaxDeliveryAge = 0 }, "max_delivery_age"},
		{"negative dispatch gap", func(c *Config) { c.MinDispatchGap = configkit.Duration(-time.Second) }, "min_dispatch_gap"},
		{"bad destination alias", func(c *Config) { c.Destinations = map[string]string{"BAD": "discord:owner"} }, "destinations"},
		{"bad destination route", func(c *Config) { c.Destinations = map[string]string{"ops": "https://evil.example"} }, "destinations[ops]"},
		{"bad fallback alias", func(c *Config) { c.Fallback = map[string]string{"BAD": "discord:owner"} }, "fallback"},
		{"bad fallback route", func(c *Config) { c.Fallback = map[string]string{"ops": "a:b:c"} }, "fallback[ops]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantPath) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.wantPath)
			}
		})
	}

	t.Run("empty fallback route and zero dispatch gap allowed", func(t *testing.T) {
		cfg := Default()
		cfg.MinDispatchGap = 0
		cfg.Destinations = map[string]string{"ops": "discord:owner"}
		cfg.Fallback = map[string]string{"ops": ""}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("valid config rejected: %v", err)
		}
	})
}
