package runtimeengine

import (
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
)

const (
	MinRunRetention = 24 * time.Hour

	maxActiveTurnsKey = "runtime.max_active_turns"
)

type Config struct {
	MaxActiveTurns  int                `json:"max_active_turns"`
	MaxPendingTurns int                `json:"max_pending_turns"`
	TurnTimeout     configkit.Duration `json:"turn_timeout"`
	ShutdownTimeout configkit.Duration `json:"shutdown_timeout"`
	RunRetention    configkit.Duration `json:"run_retention"`
}

func Default() Config {
	return Config{
		MaxActiveTurns:  4,
		MaxPendingTurns: 64,
		TurnTimeout:     configkit.Duration(5 * time.Minute),
		ShutdownTimeout: configkit.Duration(30 * time.Second),
		RunRetention:    configkit.Duration(720 * time.Hour),
	}
}

func (c Config) Validate() error {
	if c.MaxActiveTurns <= 0 {
		return configkit.Invalid("max_active_turns", "must be positive")
	}
	if c.MaxPendingTurns < c.MaxActiveTurns {
		return configkit.Invalid("max_pending_turns", "must be at least "+maxActiveTurnsKey)
	}
	if c.TurnTimeout <= 0 {
		return configkit.Invalid("turn_timeout", "must be positive")
	}
	if c.ShutdownTimeout <= 0 {
		return configkit.Invalid("shutdown_timeout", "must be positive")
	}
	if time.Duration(c.RunRetention) < MinRunRetention {
		return configkit.Invalid("run_retention", "must be at least 24h")
	}
	return nil
}
