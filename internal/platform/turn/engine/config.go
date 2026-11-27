package runtimeengine

import (
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
)

const (
	MinRunRetention = 24 * time.Hour

	maxActiveTurnsKey           = "runtime.max_active_turns"
	turnTimeoutKey              = "runtime.turn_timeout"
	maxConsecutiveToolErrorsKey = "runtime.max_consecutive_tool_errors"
)

type Config struct {
	MaxActiveTurns           int                `json:"max_active_turns"`
	MaxPendingTurns          int                `json:"max_pending_turns"`
	TurnTimeout              configkit.Duration `json:"turn_timeout"`
	AnswerReserve            configkit.Duration `json:"answer_reserve"`
	MaxConsecutiveToolErrors int                `json:"max_consecutive_tool_errors"`
	MaxToolErrorsPerTurn     int                `json:"max_tool_errors_per_turn"`
	ShutdownTimeout          configkit.Duration `json:"shutdown_timeout"`
	RunRetention             configkit.Duration `json:"run_retention"`
}

func Default() Config {
	return Config{
		MaxActiveTurns:           4,
		MaxPendingTurns:          64,
		TurnTimeout:              configkit.Duration(5 * time.Minute),
		AnswerReserve:            configkit.Duration(30 * time.Second),
		MaxConsecutiveToolErrors: 3,
		MaxToolErrorsPerTurn:     8,
		ShutdownTimeout:          configkit.Duration(30 * time.Second),
		RunRetention:             configkit.Duration(720 * time.Hour),
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
	if c.AnswerReserve <= 0 {
		return configkit.Invalid("answer_reserve", "must be positive")
	}
	if c.AnswerReserve >= c.TurnTimeout {
		return configkit.Invalid("answer_reserve", "must be less than "+turnTimeoutKey)
	}
	if c.MaxConsecutiveToolErrors < 1 {
		return configkit.Invalid("max_consecutive_tool_errors", "must be at least 1")
	}
	if c.MaxToolErrorsPerTurn < c.MaxConsecutiveToolErrors {
		return configkit.Invalid("max_tool_errors_per_turn", "must be at least "+maxConsecutiveToolErrorsKey)
	}
	if c.ShutdownTimeout <= 0 {
		return configkit.Invalid("shutdown_timeout", "must be positive")
	}
	if time.Duration(c.RunRetention) < MinRunRetention {
		return configkit.Invalid("run_retention", "must be at least 24h")
	}
	return nil
}
