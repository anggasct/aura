package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/anggasct/aura/internal/platform/configkit"
)

const (
	WebSearchProviderDuckDuckGo = "duckduckgo"
	WebSearchProviderBrave      = "brave"
	WebSearchProviderTavily     = "tavily"
	WebSearchProviderSearXNG    = "searxng"

	webSearchEndpointField = "tools.web_search.endpoint"
)

func WebSearchProviders() []string {
	return []string{WebSearchProviderDuckDuckGo, WebSearchProviderBrave, WebSearchProviderTavily, WebSearchProviderSearXNG}
}

func DefaultWebSearchCredentialRef(provider string) string {
	switch provider {
	case WebSearchProviderBrave:
		return fmt.Sprintf("env://%s_%s_%s_%s", "AURA", "BRAVE", "API", "KEY")
	case WebSearchProviderTavily:
		return fmt.Sprintf("env://%s_%s_%s_%s", "AURA", "TAVILY", "API", "KEY")
	}
	return ""
}

func (w *ToolWebSearch) SearchEndpoint() (ServiceEndpoint, error) {
	if w == nil {
		return ServiceEndpoint{}, configkit.Invalid("tools.web_search", "is not configured")
	}
	endpoint, err := parseSearchEndpoint(w.Endpoint)
	if err != nil {
		return ServiceEndpoint{}, err
	}
	return endpoint, nil
}

func parseSearchEndpoint(raw string) (ServiceEndpoint, error) {
	endpoint, err := parseServiceEndpoint(webSearchEndpointField, raw)
	if err != nil {
		return ServiceEndpoint{}, err
	}
	if endpoint.Path == "" {
		return ServiceEndpoint{}, configkit.Invalid(webSearchEndpointField, "must carry a non-empty path")
	}
	if endpoint.Loopback && endpoint.Scheme != "http" {
		return ServiceEndpoint{}, configkit.Invalid(webSearchEndpointField, "must use http for a loopback host")
	}
	return endpoint, nil
}

func validateWebSearch(webSearch *ToolWebSearch) error {
	var problems []error
	switch webSearch.Provider {
	case WebSearchProviderDuckDuckGo:
		if webSearch.CredentialRef != "" {
			problems = append(problems, configkit.Invalid("tools.web_search.credential_ref", "must be empty for provider duckduckgo"))
		}
		problems = append(problems, rejectWebSearchEndpoint(webSearch)...)
	case WebSearchProviderBrave, WebSearchProviderTavily:
		problems = append(problems, validateWebSearchCredentialRef(webSearch, true)...)
		problems = append(problems, rejectWebSearchEndpoint(webSearch)...)
	case WebSearchProviderSearXNG:
		problems = append(problems, validateWebSearchCredentialRef(webSearch, false)...)
		if _, err := parseSearchEndpoint(webSearch.Endpoint); err != nil {
			problems = append(problems, err)
		}
	default:
		problems = append(problems, configkit.Invalid("tools.web_search.provider", "must be one of "+strings.Join(WebSearchProviders(), ", ")))
	}
	if webSearch.MaxResults < 1 || webSearch.MaxResults > 20 {
		problems = append(problems, configkit.Invalid("tools.web_search.max_results", "must be between 1 and 20"))
	}
	return errors.Join(problems...)
}

func rejectWebSearchEndpoint(webSearch *ToolWebSearch) []error {
	if webSearch.Endpoint == "" {
		return nil
	}
	return []error{configkit.Invalid("tools.web_search.endpoint", "is only supported for provider searxng, not "+webSearch.Provider)}
}

func validateWebSearchCredentialRef(webSearch *ToolWebSearch, required bool) []error {
	ref := webSearch.CredentialRef
	if ref == "" {
		if required {
			return []error{configkit.Invalid("tools.web_search.credential_ref", "must use env:// or file:// with a name for provider "+webSearch.Provider)}
		}
		return nil
	}
	var problems []error
	if !credentialRefHasName(ref) {
		problems = append(problems, configkit.Invalid("tools.web_search.credential_ref", "must use env:// or file:// with a name"))
	}
	for _, other := range WebSearchProviders() {
		if other != webSearch.Provider && ref == DefaultWebSearchCredentialRef(other) {
			problems = append(problems, configkit.Invalid("tools.web_search.credential_ref", fmt.Sprintf("is the default reference of provider %s and cannot be used with provider %s", other, webSearch.Provider)))
		}
	}
	return problems
}

func credentialRefHasName(ref string) bool {
	switch {
	case strings.HasPrefix(ref, "env://"):
		return len(ref) > len("env://")
	case strings.HasPrefix(ref, "file://"):
		return len(ref) > len("file://")
	}
	return false
}
