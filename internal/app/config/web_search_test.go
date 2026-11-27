package config

import (
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/modules/search"
)

const webSearchBase = "version: 1\ntools:\n  workspace: /srv/aura/workspace\n%s\nskills:\n  roots: [/srv/aura/skills]\ncontext:\n  recent_complete_turns: 5\nprofile:\n  prompt_version: v1\nchildren:\n  recovery: interrupt\nvision: {}\n"

func webSearchConfig(section string) string {
	if section == "" {
		return strings.Replace(webSearchBase, "%s\n", "", 1)
	}
	return strings.Replace(webSearchBase, "%s", "  web_search:\n"+section, 1)
}

func loadWebSearch(t *testing.T, section string) (search.Config, error) {
	t.Helper()
	result, err := LoadWithOptions(writeTempConfig(t, webSearchConfig(section)), execLinuxOptions(t))
	if err != nil {
		return search.Config{}, err
	}
	return result.Config.Tools.WebSearch, nil
}

func TestWebSearchDefaultsToDuckDuckGoWithoutCredential(t *testing.T) {
	got, err := loadWebSearch(t, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Provider != search.ProviderDuckDuckGo || !got.CredentialRef.IsZero() || got.Endpoint != "" || got.MaxResults != 5 {
		t.Fatalf("WebSearch = %+v", got)
	}
	defaults := Default()
	if defaults.Tools.WebSearch != got {
		t.Fatalf("Default() WebSearch = %+v, loaded %+v", defaults.Tools.WebSearch, got)
	}
}

func TestWebSearchCredentialDefaultFollowsTheSelectedProvider(t *testing.T) {
	tests := map[string]struct {
		section string
		ref     string
	}{
		"brave":                  {section: "    provider: brave\n", ref: "env://AURA_BRAVE_API_KEY"},
		"tavily":                 {section: "    provider: tavily\n", ref: "env://AURA_TAVILY_API_KEY"},
		"searxng":                {section: "    provider: searxng\n    endpoint: http://127.0.0.1:8888/search\n", ref: ""},
		"duckduckgo":             {section: "    provider: duckduckgo\n", ref: ""},
		"live brave config":      {section: "    provider: brave\n    credential_ref: env://AURA_BRAVE_API_KEY\n", ref: "env://AURA_BRAVE_API_KEY"},
		"custom ref":             {section: "    provider: tavily\n    credential_ref: env://MY_TAVILY\n", ref: "env://MY_TAVILY"},
		"searxng with file ref":  {section: "    provider: searxng\n    endpoint: https://search.example.com/search\n    credential_ref: file:///run/secrets/searx\n", ref: "file:///run/secrets/searx"},
		"searxng explicit empty": {section: "    provider: searxng\n    endpoint: https://search.example.com/search\n    credential_ref: \"\"\n", ref: ""},
		"brave explicit empty":   {section: "    provider: brave\n    credential_ref: \"\"\n", ref: "env://AURA_BRAVE_API_KEY"},
		"tavily explicit empty":  {section: "    provider: tavily\n    credential_ref: \"\"\n", ref: "env://AURA_TAVILY_API_KEY"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := loadWebSearch(t, test.section)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got.CredentialRef.String() != test.ref {
				t.Fatalf("CredentialRef = %q, want %q", got.CredentialRef, test.ref)
			}
		})
	}
}

func TestWebSearchProviderAndCredentialFromEnvironment(t *testing.T) {
	t.Setenv("AURA_TOOLS_WEB_SEARCH_PROVIDER", "tavily")
	got, err := loadWebSearch(t, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Provider != search.ProviderTavily || got.CredentialRef.String() != "env://AURA_TAVILY_API_KEY" {
		t.Fatalf("WebSearch = %+v", got)
	}
}

func TestWebSearchRejectsConfigsThatDoNotFitTheProvider(t *testing.T) {
	tests := map[string]struct {
		section string
		detail  string
	}{
		"unknown provider":                {section: "    provider: bing\n", detail: "must be one of duckduckgo, brave, tavily, searxng"},
		"empty provider":                  {section: "    provider: \"\"\n", detail: "must be one of"},
		"duckduckgo endpoint":             {section: "    provider: duckduckgo\n    endpoint: https://search.example.com/search\n", detail: "only supported for provider searxng"},
		"brave endpoint":                  {section: "    provider: brave\n    endpoint: https://search.example.com/search\n", detail: "only supported for provider searxng"},
		"tavily endpoint":                 {section: "    provider: tavily\n    endpoint: https://search.example.com/search\n", detail: "only supported for provider searxng"},
		"duckduckgo credential":           {section: "    provider: duckduckgo\n    credential_ref: env://X\n", detail: "must be empty for provider duckduckgo"},
		"credential without provider":     {section: "    credential_ref: env://AURA_BRAVE_API_KEY\n", detail: "must be empty for provider duckduckgo"},
		"searxng without endpoint":        {section: "    provider: searxng\n", detail: "tools.web_search.endpoint: config_invalid: must be set"},
		"non string endpoint":             {section: "    provider: searxng\n    endpoint: 8888\n", detail: "tools.web_search.endpoint (line 6, column 15): config_invalid: expected type 'string', got unconvertible type 'int'"},
		"brave literal key":               {section: "    provider: brave\n    credential_ref: sk-literal\n", detail: "credential_ref: config_invalid: secret_scope_violation: secret reference must be empty or use the env:// or file:// scheme"},
		"brave bare env":                  {section: "    provider: brave\n    credential_ref: env://\n", detail: "credential_ref: config_invalid: secret_scope_violation: env:// secret reference must be a valid environment variable name"},
		"tavily bare file":                {section: "    provider: tavily\n    credential_ref: file://\n", detail: "credential_ref: config_invalid: secret_scope_violation: file:// secret reference must be an absolute, clean path"},
		"searxng bare env":                {section: "    provider: searxng\n    endpoint: https://search.example.com/search\n    credential_ref: env://\n", detail: "credential_ref: config_invalid: secret_scope_violation: env:// secret reference must be a valid environment variable name"},
		"searxng malformed credential":    {section: "    provider: searxng\n    endpoint: https://search.example.com/search\n    credential_ref: token\n", detail: "credential_ref: config_invalid: secret_scope_violation: secret reference must be empty or use the env:// or file:// scheme"},
		"tavily with brave default":       {section: "    provider: tavily\n    credential_ref: env://AURA_BRAVE_API_KEY\n", detail: "default reference of provider brave"},
		"searxng with brave default":      {section: "    provider: searxng\n    endpoint: https://search.example.com/search\n    credential_ref: env://AURA_BRAVE_API_KEY\n", detail: "default reference of provider brave"},
		"duckduckgo with brave default":   {section: "    provider: duckduckgo\n    credential_ref: env://AURA_BRAVE_API_KEY\n", detail: "must be empty for provider duckduckgo"},
		"brave with tavily default":       {section: "    provider: brave\n    credential_ref: env://AURA_TAVILY_API_KEY\n", detail: "default reference of provider tavily"},
		"max results above bound":         {section: "    max_results: 21\n", detail: "max_results: config_invalid: must be between 1 and 20"},
		"searxng endpoint without path":   {section: "    provider: searxng\n    endpoint: https://search.example.com\n", detail: "non-empty path"},
		"searxng endpoint root path only": {section: "    provider: searxng\n    endpoint: http://127.0.0.1:8888/\n", detail: "non-empty path"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := loadWebSearch(t, test.section)
			if err == nil || !strings.Contains(problemText(err, test.detail), test.detail) {
				t.Fatalf("error = %v, want detail %q", err, test.detail)
			}
			if code, ok := codeOf(err); ok && code != invalidCode {
				t.Fatalf("code = %q, want config_invalid", code)
			}
		})
	}
}

func TestWebSearchEndpointRules(t *testing.T) {
	accepted := map[string]string{
		"loopback http":      "http://127.0.0.1:8888/search",
		"loopback ipv6 http": "http://[::1]:8888/search",
		"https host":         "https://search.example.com/search",
		"https host port":    "https://search.example.com:8443/a/search",
		"https public ip":    "https://93.184.216.34/search",
	}
	for name, endpoint := range accepted {
		t.Run("accepts "+name, func(t *testing.T) {
			got, err := loadWebSearch(t, "    provider: searxng\n    endpoint: "+endpoint+"\n")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got.Endpoint != endpoint {
				t.Fatalf("Endpoint = %q", got.Endpoint)
			}
			parsed, err := got.SearchEndpoint()
			if err != nil {
				t.Fatalf("SearchEndpoint: %v", err)
			}
			if parsed.Loopback != strings.HasPrefix(endpoint, "http://") {
				t.Fatalf("Loopback = %v for %q", parsed.Loopback, endpoint)
			}
		})
	}
	rejected := map[string]string{
		"http localhost":         "http://localhost:8888/search",
		"http private ip":        "http://10.0.0.5/search",
		"http remote host":       "http://search.example.com/search",
		"loopback lookalike":     "http://127.0.0.2:8888/search",
		"loopback shorthand":     "http://127.1:8888/search",
		"https loopback":         "https://127.0.0.1:8888/search",
		"https localhost":        "https://localhost/search",
		"https private ip":       "https://10.0.0.5/search",
		"https metadata ip":      "https://169.254.169.254/search",
		"query":                  "https://search.example.com/search?x=1",
		"fragment":               "https://search.example.com/search#x",
		"credentials":            "https://user:pw@search.example.com/search",
		"empty path":             "https://search.example.com",
		"loopback empty path":    "http://127.0.0.1:8888",
		"unsupported scheme":     "ftp://search.example.com/search",
		"encoded path":           "https://search.example.com/a%2Fb",
		"bad port":               "https://search.example.com:0/search",
		"surrounding whitespace": " https://search.example.com/search",
	}
	for name, endpoint := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			_, err := loadWebSearch(t, "    provider: searxng\n    endpoint: \""+endpoint+"\"\n")
			if err == nil {
				t.Fatalf("Load accepted %q", endpoint)
			}
			if code, ok := codeOf(err); !ok || code != invalidCode {
				t.Fatalf("error = %v, want config_invalid", err)
			}
		})
	}
}

func TestWebSearchEndpointFromEnvironment(t *testing.T) {
	t.Setenv("AURA_TOOLS_WEB_SEARCH_PROVIDER", "searxng")
	t.Setenv("AURA_TOOLS_WEB_SEARCH_ENDPOINT", "http://127.0.0.1:9999/search")
	got, err := loadWebSearch(t, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Endpoint != "http://127.0.0.1:9999/search" {
		t.Fatalf("Endpoint = %q", got.Endpoint)
	}
	endpoint, err := got.SearchEndpoint()
	if err != nil || endpoint.URL() != "http://127.0.0.1:9999/search" || endpoint.Authority() != "127.0.0.1:9999" || !endpoint.Loopback {
		t.Fatalf("SearchEndpoint = %+v, %v", endpoint, err)
	}
}

func TestWebSearchEndpointIsNotWrittenWhenEmpty(t *testing.T) {
	defaults := Default()
	data, err := Marshal(&defaults)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	_, rest, found := strings.Cut(string(data), "  web_search:\n")
	if !found {
		t.Fatalf("web_search section missing:\n%s", data)
	}
	section, _, _ := strings.Cut(rest, "agents:")
	if strings.Contains(section, "endpoint") || strings.Contains(section, "credential_ref") {
		t.Fatalf("empty web_search fields were written:\n%s", section)
	}
}
