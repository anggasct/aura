package plugin

import (
	"errors"
	"fmt"
	"regexp"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/configkit"
)

const (
	MaxVersionLength = 64
	MaxDesired       = 256
)

var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[a-zA-Z-][0-9a-zA-Z-]*)(\.(0|[1-9][0-9]*|[0-9]*[a-zA-Z-][0-9a-zA-Z-]*))*))?(\+[0-9a-zA-Z-]+(\.[0-9a-zA-Z-]+)*)?$`)

type Config struct {
	Desired []Desired `json:"desired"`
}

type Desired struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Enabled bool   `json:"enabled"`
}

func ValidVersion(version string) bool {
	return len(version) <= MaxVersionLength && versionPattern.MatchString(version)
}

func (c *Config) Validate() error {
	if c == nil {
		return nil
	}
	var errs []error
	if len(c.Desired) > MaxDesired {
		errs = append(errs, configkit.Invalid("desired", fmt.Sprintf("lists more than %d plugins", MaxDesired)))
	}
	seen := make(map[string]struct{}, len(c.Desired))
	for i := range c.Desired {
		entry := &c.Desired[i]
		if !auraagent.ValidPluginID(entry.ID) {
			errs = append(errs, configkit.Invalid(fmt.Sprintf("desired[%d].id", i), "must match [a-z][a-z0-9-]{1,47}"))
		}
		if !ValidVersion(entry.Version) {
			errs = append(errs, configkit.Invalid(fmt.Sprintf("desired[%d].version", i), "must be a semantic version"))
		}
		if _, duplicate := seen[entry.ID]; duplicate {
			errs = append(errs, configkit.Invalid("desired", fmt.Sprintf("lists plugin %q twice", entry.ID)))
		}
		seen[entry.ID] = struct{}{}
	}
	return errors.Join(errs...)
}
