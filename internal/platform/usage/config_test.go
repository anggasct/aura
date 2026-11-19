package usage

import (
	"testing"
)

func TestUsageConfigValidate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"currency empty", func(c *Config) { c.Currency = "" }, "currency: config_invalid: must not be empty"},
		{"daily budget negative", func(c *Config) { c.DailyBudgetMicros = -1 }, "daily_budget_micros: config_invalid: must not be negative"},
		{"monthly budget negative", func(c *Config) { c.MonthlyBudgetMicros = -1 }, "monthly_budget_micros: config_invalid: must not be negative"},
		{"reservation ttl zero", func(c *Config) { c.ReservationTTL = 0 }, "reservation_ttl: config_invalid: must be positive"},
		{"reservation ttl negative", func(c *Config) { c.ReservationTTL = -1 }, "reservation_ttl: config_invalid: must be positive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || err.Error() != tc.want {
				t.Errorf("Validate() = %v, want %q", err, tc.want)
			}
		})
	}
	if err := Default().Validate(); err != nil {
		t.Errorf("Default().Validate() = %v, want nil", err)
	}
}
