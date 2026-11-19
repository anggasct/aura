package usage

import (
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
)

type Config struct {
	Currency            string             `json:"currency"`
	DailyBudgetMicros   int64              `json:"daily_budget_micros"`
	MonthlyBudgetMicros int64              `json:"monthly_budget_micros"`
	ReservationTTL      configkit.Duration `json:"reservation_ttl"`
}

func Default() Config {
	return Config{
		Currency:            "USD",
		DailyBudgetMicros:   10000000,
		MonthlyBudgetMicros: 200000000,
		ReservationTTL:      configkit.Duration(time.Hour),
	}
}

func (c Config) Validate() error {
	if c.Currency == "" {
		return configkit.Invalid("currency", "must not be empty")
	}
	if c.DailyBudgetMicros < 0 {
		return configkit.Invalid("daily_budget_micros", "must not be negative")
	}
	if c.MonthlyBudgetMicros < 0 {
		return configkit.Invalid("monthly_budget_micros", "must not be negative")
	}
	if c.ReservationTTL <= 0 {
		return configkit.Invalid("reservation_ttl", "must be positive")
	}
	return nil
}
