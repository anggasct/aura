package capability

import (
	"errors"
	"strconv"

	"github.com/anggasct/aura/internal/platform/configkit"
)

type Config struct {
	Enabled []string `json:"enabled"`
}

func Default() Config {
	return Config{Enabled: []string{}}
}

func (c Config) Validate() error {
	var problems []error
	seen := make(map[string]bool, len(c.Enabled))
	for i, name := range c.Enabled {
		if seen[name] {
			problems = append(problems, configkit.Invalid("enabled["+strconv.Itoa(i)+"]", "is listed more than once"))
		}
		seen[name] = true
	}
	return errors.Join(problems...)
}
