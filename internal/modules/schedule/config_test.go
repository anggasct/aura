package schedule

import (
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
)

func TestScheduleConfigDefaultValidates(t *testing.T) {
	cfg := Default()
	if !cfg.Enabled || cfg.DefaultTimezone != "UTC" ||
		cfg.DefaultCatchUpGrace != configkit.Duration(15*time.Minute) ||
		cfg.OccurrenceRetention != configkit.Duration(720*time.Hour) {
		t.Fatalf("Default() = %+v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default().Validate(): %v", err)
	}
}

func TestScheduleConfigValidate(t *testing.T) {
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
		{"empty timezone", func(c *Config) { c.DefaultTimezone = "" }, "default_timezone"},
		{"invalid timezone", func(c *Config) { c.DefaultTimezone = "Mars/Olympus" }, "default_timezone"},
		{"negative catch-up grace", func(c *Config) { c.DefaultCatchUpGrace = configkit.Duration(-time.Second) }, "default_catch_up_grace"},
		{"zero retention", func(c *Config) { c.OccurrenceRetention = 0 }, "occurrence_retention"},
		{"negative retention", func(c *Config) { c.OccurrenceRetention = configkit.Duration(-time.Minute) }, "occurrence_retention"},
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

	t.Run("zero catch-up grace allowed", func(t *testing.T) {
		cfg := Default()
		cfg.DefaultCatchUpGrace = 0
		if err := cfg.Validate(); err != nil {
			t.Fatalf("zero catch-up grace rejected: %v", err)
		}
	})
}
