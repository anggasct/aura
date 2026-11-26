package terminal

import (
	"errors"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
)

type Config struct {
	RenderHz            int                `json:"render_hz"`
	MaxInputBytes       int                `json:"max_input_bytes"`
	InMemoryHistory     int                `json:"in_memory_history"`
	SecondInterruptTime configkit.Duration `json:"second_interrupt_window"`
	PlainApproval       string             `json:"plain_approval"`
}

func Default() Config {
	return Config{
		RenderHz:            20,
		MaxInputBytes:       262144,
		InMemoryHistory:     100,
		SecondInterruptTime: configkit.Duration(2 * time.Second),
		PlainApproval:       "deny",
	}
}

func (c Config) Validate() error {
	var problems []error
	if c.RenderHz <= 0 {
		problems = append(problems, configkit.Invalid("render_hz", "must be positive"))
	}
	if c.MaxInputBytes <= 0 {
		problems = append(problems, configkit.Invalid("max_input_bytes", "must be positive"))
	}
	if c.InMemoryHistory < 0 {
		problems = append(problems, configkit.Invalid("in_memory_history", "must not be negative"))
	}
	if c.SecondInterruptTime <= 0 {
		problems = append(problems, configkit.Invalid("second_interrupt_window", "must be positive"))
	}
	switch c.PlainApproval {
	case "deny":
	default:
		problems = append(problems, configkit.Invalid("plain_approval", "must be \"deny\""))
	}
	return errors.Join(problems...)
}
