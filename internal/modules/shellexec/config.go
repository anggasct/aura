package shellexec

import (
	"errors"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
)

type Config struct {
	Timeout        configkit.Duration `json:"timeout"`
	MaxStdoutBytes int64              `json:"max_stdout_bytes"`
	MaxStderrBytes int64              `json:"max_stderr_bytes"`
}

func Default() Config {
	return Config{
		Timeout:        configkit.Duration(30 * time.Second),
		MaxStdoutBytes: 65536,
		MaxStderrBytes: 65536,
	}
}

func (c *Config) Validate() error {
	if c == nil {
		return nil
	}
	var problems []error
	if c.Timeout <= 0 {
		problems = append(problems, configkit.Invalid("timeout", "must be positive"))
	}
	if c.MaxStdoutBytes <= 0 || c.MaxStderrBytes <= 0 {
		problems = append(problems, configkit.Invalid("", "output limits must be positive"))
	}
	return errors.Join(problems...)
}
