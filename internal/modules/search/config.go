package search

import (
	"errors"
	"fmt"
	"strings"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/secret"
)

const (
	ProviderDuckDuckGo = "duckduckgo"
	ProviderBrave      = "brave"
	ProviderTavily     = "tavily"
	ProviderSearXNG    = "searxng"

	endpointField = "endpoint"
)

var (
	defaultBraveCredentialRef  = mustRef(fmt.Sprintf("env://%s_%s_%s_%s", "AURA", "BRAVE", "API", "KEY"))
	defaultTavilyCredentialRef = mustRef(fmt.Sprintf("env://%s_%s_%s_%s", "AURA", "TAVILY", "API", "KEY"))
)

type Config struct {
	Provider      string     `json:"provider"`
	CredentialRef secret.Ref `json:"credential_ref,omitzero"`
	Endpoint      string     `json:"endpoint,omitempty"`
	MaxResults    int        `json:"max_results"`
}

func Default() Config {
	return Config{
		Provider:   ProviderDuckDuckGo,
		MaxResults: 5,
	}
}

func Providers() []string {
	return []string{ProviderDuckDuckGo, ProviderBrave, ProviderTavily, ProviderSearXNG}
}

func DefaultCredentialRef(provider string) secret.Ref {
	switch provider {
	case ProviderBrave:
		return defaultBraveCredentialRef
	case ProviderTavily:
		return defaultTavilyCredentialRef
	}
	return secret.Ref{}
}

func (c *Config) SearchEndpoint() (egress.ServiceEndpoint, error) {
	if c == nil {
		return egress.ServiceEndpoint{}, configkit.Invalid("", "is not configured")
	}
	endpoint, err := parseSearchEndpoint(c.Endpoint)
	if err != nil {
		return egress.ServiceEndpoint{}, err
	}
	return endpoint, nil
}

func (c *Config) Validate() error {
	if c == nil {
		return nil
	}
	var problems []error
	switch c.Provider {
	case ProviderDuckDuckGo:
		if !c.CredentialRef.IsZero() {
			problems = append(problems, configkit.Invalid("credential_ref", "must be empty for provider duckduckgo"))
		}
		problems = append(problems, rejectConfigEndpoint(c)...)
	case ProviderBrave, ProviderTavily:
		problems = append(problems, validateCredentialRef(c, true)...)
		problems = append(problems, rejectConfigEndpoint(c)...)
	case ProviderSearXNG:
		problems = append(problems, validateCredentialRef(c, false)...)
		if _, err := parseSearchEndpoint(c.Endpoint); err != nil {
			problems = append(problems, err)
		}
	default:
		problems = append(problems, configkit.Invalid("provider", "must be one of "+strings.Join(Providers(), ", ")))
	}
	if c.MaxResults < 1 || c.MaxResults > 20 {
		problems = append(problems, configkit.Invalid("max_results", "must be between 1 and 20"))
	}
	return errors.Join(problems...)
}

func (c *Config) ConfiguredSecrets() []string {
	if c == nil {
		return nil
	}
	value, err := c.CredentialRef.Reference().Resolve()
	if err != nil || value == "" {
		return nil
	}
	return []string{value}
}

func parseSearchEndpoint(raw string) (egress.ServiceEndpoint, error) {
	endpoint, err := egress.ParseServiceEndpoint(raw)
	if err != nil {
		return egress.ServiceEndpoint{}, configkit.Invalid(endpointField, err.Error())
	}
	if endpoint.Path == "" {
		return egress.ServiceEndpoint{}, configkit.Invalid(endpointField, "must carry a non-empty path")
	}
	if endpoint.Loopback && endpoint.Scheme != "http" {
		return egress.ServiceEndpoint{}, configkit.Invalid(endpointField, "must use http for a loopback host")
	}
	return endpoint, nil
}

func rejectConfigEndpoint(cfg *Config) []error {
	if cfg.Endpoint == "" {
		return nil
	}
	return []error{configkit.Invalid(endpointField, "is only supported for provider searxng, not "+cfg.Provider)}
}

func validateCredentialRef(cfg *Config, required bool) []error {
	ref := cfg.CredentialRef
	if ref.IsZero() {
		if required {
			return []error{configkit.Invalid("credential_ref", "must use env:// or file:// with a name for provider "+cfg.Provider)}
		}
		return nil
	}
	var problems []error
	for _, other := range Providers() {
		if other != cfg.Provider && ref == DefaultCredentialRef(other) {
			problems = append(problems, configkit.Invalid("credential_ref", fmt.Sprintf("is the default reference of provider %s and cannot be used with provider %s", other, cfg.Provider)))
		}
	}
	return problems
}

func mustRef(text string) secret.Ref {
	ref, err := secret.ParseRef(text)
	if err != nil {
		panic(err)
	}
	return ref
}
