package logging

import (
	"errors"

	"github.com/anggasct/aura/internal/platform/configkit"
)

type Config struct {
	Level  string `json:"level"`
	Format string `json:"format"`
}

func Default() Config {
	return Config{
		Level:  "info",
		Format: "text",
	}
}

func (c Config) Validate() error {
	var problems []error
	if _, err := ParseLevel(c.Level); err != nil {
		problems = append(problems, configkit.Invalid("level", "must be one of debug, info, warn, error"))
	}
	if _, err := ParseFormat(c.Format); err != nil {
		problems = append(problems, configkit.Invalid("format", "must be one of text, json"))
	}
	return errors.Join(problems...)
}
