package github

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/secret"
)

var credentialNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

type Config struct {
	Credentials []Credential `json:"credentials,omitempty"`
}

func Default() Config {
	return Config{}
}

func (c *Config) Validate() error {
	if c == nil {
		return nil
	}
	names := make(map[string]bool, len(c.Credentials))
	refs := make(map[secret.Ref]bool, len(c.Credentials))
	var errs []error
	for i := range c.Credentials {
		entry := &c.Credentials[i]
		path := fmt.Sprintf("credentials[%d]", i)
		switch {
		case !credentialNamePattern.MatchString(entry.Name):
			errs = append(errs, configkit.Invalid(path+".name", "must be 1 to 64 lowercase letters, digits, '_' or '-', starting with a letter"))
		case names[entry.Name]:
			errs = append(errs, configkit.Invalid(path+".name", "repeats an earlier entry"))
		case entry.Ref.IsZero():
			errs = append(errs, configkit.Invalid(path+".ref", "must name an environment variable or an absolute, clean file path"))
		case refs[entry.Ref]:
			errs = append(errs, configkit.Invalid(path+".ref", "repeats an earlier entry"))
		default:
			names[entry.Name] = true
			refs[entry.Ref] = true
		}
	}
	return errors.Join(errs...)
}

func (c *Config) SecretValues() []string {
	if c == nil {
		return nil
	}
	var values []string
	for _, credential := range c.Credentials {
		if value, err := credential.Ref.Reference().Resolve(); err == nil && strings.TrimSpace(value) != "" {
			values = append(values, value)
		}
	}
	return values
}
