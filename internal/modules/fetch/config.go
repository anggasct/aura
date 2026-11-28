package fetch

import (
	"errors"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
)

type Config struct {
	Timeout         configkit.Duration `json:"timeout"`
	MaxRedirects    int                `json:"max_redirects"`
	MaxEncodedBytes int64              `json:"max_encoded_bytes"`
	MaxDecodedBytes int64              `json:"max_decoded_bytes"`

	WebOnlyFetchesPerRun int `json:"web_only_fetches_per_run"`
}

const MaxWebOnlyFetchesPerRun = 50

func Default() Config {
	return Config{
		Timeout:         configkit.Duration(30 * time.Second),
		MaxRedirects:    5,
		MaxEncodedBytes: 2097152,
		MaxDecodedBytes: 8388608,

		WebOnlyFetchesPerRun: 10,
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
	if c.MaxRedirects < 0 || c.MaxRedirects > 10 {
		problems = append(problems, configkit.Invalid("max_redirects", "must be between 0 and 10"))
	}
	if c.MaxEncodedBytes <= 0 || c.MaxDecodedBytes <= 0 {
		problems = append(problems, configkit.Invalid("", "body limits must be positive"))
	}
	if c.WebOnlyFetchesPerRun < 0 || c.WebOnlyFetchesPerRun > MaxWebOnlyFetchesPerRun {
		problems = append(problems, configkit.Invalid("web_only_fetches_per_run", "must be between 0 and 50"))
	}
	return errors.Join(problems...)
}
