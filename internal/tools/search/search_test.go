package search

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/egress"
	"github.com/anggasct/aura/internal/toolbroker"
)

type staticResolver map[string][]net.IP

func (r staticResolver) LookupIP(_ context.Context, host string) ([]net.IP, error) {
	ips, ok := r[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host}
	}
	return ips, nil
}

type recordingResolver struct {
	staticResolver
	mu    sync.Mutex
	hosts []string
}

func (r *recordingResolver) LookupIP(ctx context.Context, host string) ([]net.IP, error) {
	r.mu.Lock()
	r.hosts = append(r.hosts, host)
	r.mu.Unlock()
	return r.staticResolver.LookupIP(ctx, host)
}

func (r *recordingResolver) lookups() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.hosts)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type capturedRequest struct {
	method string
	url    *url.URL
	header http.Header
	body   []byte
	calls  int
}

func cannedClient(status int, body string, captured *capturedRequest) *http.Client {
	return &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if captured != nil {
			captured.calls++
			captured.method = req.Method
			captured.url = req.URL
			captured.header = req.Header.Clone()
			if req.Body != nil {
				captured.body, _ = io.ReadAll(req.Body)
			}
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}
}

func failingClient(err error) *http.Client {
	return &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, err
	})}
}

func searchResolver() staticResolver {
	return staticResolver{
		"search.example":   {net.ParseIP("93.184.216.34")},
		"internal.example": {net.ParseIP("10.0.0.5")},
	}
}

const (
	searxngEndpoint = "https://search.example/search"
	testCredential  = "search-secret"
)

func optionsFor(provider string) *Options {
	options := &Options{Provider: provider, Timeout: time.Second, MaxResults: 5, MaxBodyBytes: 1 << 20, Resolver: searchResolver()}
	switch provider {
	case providerBrave, providerTavily:
		options.CredentialRef = "env://AURA_TEST_SEARCH_TOKEN"
	case providerSearXNG:
		options.Endpoint = searxngEndpoint
	}
	return options
}

func newAdapter(t *testing.T, options *Options) toolbroker.Adapter {
	t.Helper()
	adapter, err := New(options)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return adapter
}

func callSearch(adapter toolbroker.Adapter, arguments string) (toolbroker.ToolResult, error) {
	return adapter(context.Background(), &toolbroker.ToolRequest{ToolName: "web_search", ToolVersion: "v1", Arguments: []byte(arguments)}, approval.Constraints{})
}

func decodeResults(t *testing.T, output []byte) []searchResult {
	t.Helper()
	var decoded result
	if err := json.Unmarshal(output, &decoded); err != nil {
		t.Fatalf("decode %s: %v", output, err)
	}
	return decoded.Results
}

func classOf(err error) toolbroker.ResultClass {
	class, _ := toolbroker.CodeOf(err)
	return class
}

func detailOf(err error) string {
	var target *toolbroker.Error
	if errors.As(err, &target) {
		return target.Detail
	}
	return ""
}

const (
	braveFixture = `{"web":{"results":[
		{"title":"Go","url":"https://go.dev","description":"The Go language"},
		{"title":"No URL","url":"","description":"dropped"},
		{"title":"Pkg","url":"https://pkg.go.dev","description":"Packages"},
		{"title":"Blog","url":"https://go.dev/blog","description":"Blog"}]}}`
	tavilyFixture = `{"query":"go","results":[
		{"title":"Go","url":"https://go.dev","content":"The Go language","score":0.9},
		{"title":"No URL","content":"dropped"},
		{"title":"Pkg","url":"https://pkg.go.dev","content":"Packages"},
		{"title":"Blog","url":"https://go.dev/blog","content":"Blog"}]}`
	searxngFixture = `{"query":"go","results":[
		{"title":"Go","url":"https://go.dev","content":"The Go language","engine":"duckduckgo"},
		{"title":"No URL","url":"","content":"dropped"},
		{"title":"Pkg","url":"https://pkg.go.dev","content":"Packages"},
		{"title":"Blog","url":"https://go.dev/blog","content":"Blog"}],"unresponsive_engines":[["brave","timeout"]]}`
)

func TestEveryProviderProducesTheSameResultShapeCutToTheLimit(t *testing.T) {
	t.Setenv("AURA_TEST_SEARCH_TOKEN", testCredential)
	jsonWant := []searchResult{
		{Title: "Go", URL: "https://go.dev", Snippet: "The Go language"},
		{Title: "Pkg", URL: "https://pkg.go.dev", Snippet: "Packages"},
	}
	tests := map[string]struct {
		fixture string
		want    []searchResult
	}{
		providerDuckDuckGo: {fixture: duckDuckGoFixture, want: []searchResult{
			{Title: "Go Programming Language", URL: "https://go.dev/", Snippet: "The Go language builds simple software."},
			{Title: "context package", URL: "https://pkg.go.dev/context"},
		}},
		providerBrave:   {fixture: braveFixture, want: jsonWant},
		providerTavily:  {fixture: tavilyFixture, want: jsonWant},
		providerSearXNG: {fixture: searxngFixture, want: jsonWant},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			options := optionsFor(name)
			options.client = cannedClient(http.StatusOK, test.fixture, nil)
			got, err := callSearch(newAdapter(t, options), `{"query":"go","max_results":2}`)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if got.Class != toolbroker.ResultOK {
				t.Fatalf("class = %q", got.Class)
			}
			results := decodeResults(t, got.Output)
			if len(results) != len(test.want) {
				t.Fatalf("results = %+v, want %+v", results, test.want)
			}
			for i := range test.want {
				if results[i] != test.want[i] {
					t.Fatalf("result %d = %+v, want %+v", i, results[i], test.want[i])
				}
			}
		})
	}
}

func TestEveryProviderSendsItsExactRequest(t *testing.T) {
	t.Setenv("AURA_TEST_SEARCH_TOKEN", testCredential)
	const query = "go & rust: ünï cödé +100%"
	arguments, err := json.Marshal(map[string]any{"query": query, "max_results": 3})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	tests := map[string]struct {
		fixture string
		check   func(t *testing.T, got *capturedRequest)
	}{
		providerDuckDuckGo: {fixture: duckDuckGoFixture, check: func(t *testing.T, got *capturedRequest) {
			requireRequest(t, got, http.MethodGet, "html.duckduckgo.com", "/html/")
			requireQuery(t, got, url.Values{"q": {query}})
			requireNoHeader(t, got, "Authorization", "X-Subscription-Token", "Content-Type")
			if got.header.Get("Accept") != "text/html" {
				t.Fatalf("Accept = %q", got.header.Get("Accept"))
			}
		}},
		providerBrave: {fixture: braveFixture, check: func(t *testing.T, got *capturedRequest) {
			requireRequest(t, got, http.MethodGet, "api.search.brave.com", "/res/v1/web/search")
			requireQuery(t, got, url.Values{"q": {query}, "count": {"3"}})
			requireNoHeader(t, got, "Authorization", "Content-Type")
			if got.header.Get("X-Subscription-Token") != testCredential || got.header.Get("Accept") != "application/json" {
				t.Fatalf("headers = %v", got.header)
			}
		}},
		providerTavily: {fixture: tavilyFixture, check: func(t *testing.T, got *capturedRequest) {
			requireRequest(t, got, http.MethodPost, "api.tavily.com", "/search")
			requireQuery(t, got, url.Values{})
			requireNoHeader(t, got, "X-Subscription-Token")
			if got.header.Get("Authorization") != "Bearer "+testCredential || got.header.Get("Content-Type") != "application/json" || got.header.Get("Accept") != "application/json" {
				t.Fatalf("headers = %v", got.header)
			}
			var body map[string]any
			if err := json.Unmarshal(got.body, &body); err != nil {
				t.Fatalf("body %s: %v", got.body, err)
			}
			if len(body) != 3 || body["query"] != query || body["max_results"] != float64(3) || body["search_depth"] != "basic" {
				t.Fatalf("body = %v", body)
			}
		}},
		providerSearXNG: {fixture: searxngFixture, check: func(t *testing.T, got *capturedRequest) {
			requireRequest(t, got, http.MethodGet, "search.example", "/search")
			requireQuery(t, got, url.Values{"q": {query}, "format": {"json"}})
			requireNoHeader(t, got, "Authorization", "X-Subscription-Token", "Content-Type")
			if got.header.Get("Accept") != "application/json" {
				t.Fatalf("Accept = %q", got.header.Get("Accept"))
			}
		}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var captured capturedRequest
			options := optionsFor(name)
			options.client = cannedClient(http.StatusOK, test.fixture, &captured)
			if _, err := callSearch(newAdapter(t, options), string(arguments)); err != nil {
				t.Fatalf("search: %v", err)
			}
			if captured.calls != 1 {
				t.Fatalf("requests = %d", captured.calls)
			}
			if strings.Contains(captured.url.String(), testCredential) {
				t.Fatal("the credential reached the URL")
			}
			if captured.header.Get("User-Agent") != "Aura (+https://github.com/anggasct/aura)" {
				t.Fatalf("User-Agent = %q", captured.header.Get("User-Agent"))
			}
			test.check(t, &captured)
		})
	}
}

func requireRequest(t *testing.T, got *capturedRequest, method, host, path string) {
	t.Helper()
	if got.method != method || got.url.Scheme != "https" || got.url.Host != host || got.url.Path != path || got.url.User != nil || got.url.Fragment != "" {
		t.Fatalf("request = %s %s", got.method, got.url)
	}
}

func requireQuery(t *testing.T, got *capturedRequest, want url.Values) {
	t.Helper()
	have := got.url.Query()
	if len(have) != len(want) {
		t.Fatalf("query = %v, want %v", have, want)
	}
	for key, values := range want {
		if len(have[key]) != 1 || have[key][0] != values[0] {
			t.Fatalf("query %s = %v, want %v", key, have[key], values)
		}
	}
}

func requireNoHeader(t *testing.T, got *capturedRequest, names ...string) {
	t.Helper()
	for _, name := range names {
		if got.header.Get(name) != "" {
			t.Fatalf("header %s = %q, want none", name, got.header.Get(name))
		}
	}
}

func TestSearXNGSendsBearerOnlyWhenACredentialIsConfigured(t *testing.T) {
	t.Setenv("AURA_TEST_SEARCH_TOKEN", testCredential)
	for name, ref := range map[string]string{"without credential": "", "with credential": "env://AURA_TEST_SEARCH_TOKEN"} {
		t.Run(name, func(t *testing.T) {
			var captured capturedRequest
			options := optionsFor(providerSearXNG)
			options.CredentialRef = ref
			options.client = cannedClient(http.StatusOK, searxngFixture, &captured)
			if _, err := callSearch(newAdapter(t, options), `{"query":"go"}`); err != nil {
				t.Fatalf("search: %v", err)
			}
			got := captured.header.Get("Authorization")
			if ref == "" && got != "" {
				t.Fatalf("Authorization = %q, want none", got)
			}
			if ref != "" && got != "Bearer "+testCredential {
				t.Fatalf("Authorization = %q", got)
			}
		})
	}
}

func TestCredentialedProvidersNeedAResolvableCredential(t *testing.T) {
	t.Setenv("AURA_TEST_EMPTY_TOKEN", "")
	refs := map[string]string{
		"empty reference":      "",
		"unset variable":       "env://AURA_TEST_SEARCH_UNSET",
		"empty variable value": "env://AURA_TEST_EMPTY_TOKEN",
		"missing file":         "file:///nonexistent/aura-search-token",
		"malformed reference":  "literal-key",
		"bare scheme":          "env://",
	}
	for _, provider := range []string{providerBrave, providerTavily, providerSearXNG} {
		for name, ref := range refs {
			if provider == providerSearXNG && ref == "" {
				continue
			}
			t.Run(provider+"/"+name, func(t *testing.T) {
				var captured capturedRequest
				options := optionsFor(provider)
				options.CredentialRef = ref
				options.client = cannedClient(http.StatusOK, braveFixture, &captured)
				_, err := callSearch(newAdapter(t, options), `{"query":"go"}`)
				if classOf(err) != toolbroker.ResultCapabilityUnavailable || captured.calls != 0 {
					t.Fatalf("err = %v, requests = %d", err, captured.calls)
				}
			})
		}
	}
}

func TestDuckDuckGoNeverResolvesACredential(t *testing.T) {
	var captured capturedRequest
	options := optionsFor(providerDuckDuckGo)
	options.client = cannedClient(http.StatusOK, duckDuckGoFixture, &captured)
	if _, err := callSearch(newAdapter(t, options), `{"query":"go"}`); err != nil {
		t.Fatalf("search: %v", err)
	}
	requireNoHeader(t, &captured, "Authorization", "X-Subscription-Token")
}

func TestStatusMappingNamesTheClassAndNeverEchoesTheProviderBody(t *testing.T) {
	t.Setenv("AURA_TEST_SEARCH_TOKEN", testCredential)
	const marker = "provider-body-marker-4f9a"
	tests := map[int]struct {
		class  toolbroker.ResultClass
		detail string
	}{
		http.StatusUnauthorized:        {toolbroker.ResultCapabilityUnavailable, "search provider refused the request"},
		http.StatusForbidden:           {toolbroker.ResultCapabilityUnavailable, "search provider refused the request"},
		http.StatusBadRequest:          {toolbroker.ResultInvalidArgument, "search provider rejected the query"},
		http.StatusUnprocessableEntity: {toolbroker.ResultInvalidArgument, "search provider rejected the query"},
		http.StatusTooManyRequests:     {toolbroker.ResultExecutionFailed, "search provider rate or usage limit reached"},
		432:                            {toolbroker.ResultExecutionFailed, "search provider rate or usage limit reached"},
		433:                            {toolbroker.ResultExecutionFailed, "search provider rate or usage limit reached"},
		http.StatusNotFound:            {toolbroker.ResultExecutionFailed, "search provider returned status 404"},
		http.StatusInternalServerError: {toolbroker.ResultExecutionFailed, "search provider returned status 500"},
		http.StatusBadGateway:          {toolbroker.ResultExecutionFailed, "search provider returned status 502"},
		http.StatusFound:               {toolbroker.ResultExecutionFailed, "search provider returned status 302"},
		http.StatusMovedPermanently:    {toolbroker.ResultExecutionFailed, "search provider returned status 301"},
	}
	for status, test := range tests {
		for _, provider := range []string{providerDuckDuckGo, providerBrave, providerTavily, providerSearXNG} {
			t.Run(provider+"/"+http.StatusText(status), func(t *testing.T) {
				options := optionsFor(provider)
				options.client = cannedClient(status, marker, nil)
				got, err := callSearch(newAdapter(t, options), `{"query":"go"}`)
				if classOf(err) != test.class || detailOf(err) != test.detail {
					t.Fatalf("err = %v, want %s: %s", err, test.class, test.detail)
				}
				if strings.Contains(err.Error(), marker) || strings.Contains(string(got.Output), marker) {
					t.Fatal("the provider body reached the result or the error")
				}
			})
		}
	}
}

func TestSearXNGSignalsFailedEnginesInsteadOfAnEmptyList(t *testing.T) {
	tests := map[string]struct {
		body   string
		failed bool
	}{
		"empty with unresponsive engines": {body: `{"results":[],"unresponsive_engines":[["google","timeout"]]}`, failed: true},
		"missing results with engines":    {body: `{"unresponsive_engines":[["google","timeout"],["brave","error"]]}`, failed: true},
		"empty with none":                 {body: `{"results":[],"unresponsive_engines":[]}`},
		"empty without the key":           {body: `{"results":[]}`},
		"results with unresponsive":       {body: `{"results":[{"title":"t","url":"https://example.com","content":"c"}],"unresponsive_engines":[["google","timeout"]]}`},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			options := optionsFor(providerSearXNG)
			options.client = cannedClient(http.StatusOK, test.body, nil)
			got, err := callSearch(newAdapter(t, options), `{"query":"go"}`)
			if test.failed {
				if classOf(err) != toolbroker.ResultExecutionFailed || detailOf(err) != "search engines did not respond" {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil || got.Class != toolbroker.ResultOK {
				t.Fatalf("got = %+v, err = %v", got, err)
			}
		})
	}
}

func TestJSONProvidersReturnAnEmptyListWhenNothingMatched(t *testing.T) {
	t.Setenv("AURA_TEST_SEARCH_TOKEN", testCredential)
	for provider, body := range map[string]string{providerBrave: `{"web":{"results":[]}}`, providerTavily: `{"results":[]}`, providerSearXNG: `{"results":[]}`} {
		t.Run(provider, func(t *testing.T) {
			options := optionsFor(provider)
			options.client = cannedClient(http.StatusOK, body, nil)
			got, err := callSearch(newAdapter(t, options), `{"query":"go"}`)
			if err != nil || string(got.Output) != `{"results":[]}` {
				t.Fatalf("output = %s, err = %v", got.Output, err)
			}
		})
	}
}

func TestMalformedProviderResponsesAreExecutionFailures(t *testing.T) {
	t.Setenv("AURA_TEST_SEARCH_TOKEN", testCredential)
	for _, provider := range []string{providerBrave, providerTavily, providerSearXNG} {
		t.Run(provider, func(t *testing.T) {
			options := optionsFor(provider)
			options.client = cannedClient(http.StatusOK, "<html>marker-not-json</html>", nil)
			_, err := callSearch(newAdapter(t, options), `{"query":"go"}`)
			if classOf(err) != toolbroker.ResultExecutionFailed || strings.Contains(err.Error(), "marker-not-json") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestOversizedProviderResponseIsRejected(t *testing.T) {
	options := optionsFor(providerSearXNG)
	options.MaxBodyBytes = 16
	options.client = cannedClient(http.StatusOK, searxngFixture, nil)
	_, err := callSearch(newAdapter(t, options), `{"query":"go"}`)
	if classOf(err) != toolbroker.ResultExecutionFailed {
		t.Fatalf("err = %v", err)
	}
}

func TestCredentialValuesAreRedactedFromResultsForEveryCredentialedProvider(t *testing.T) {
	t.Setenv("AURA_TEST_SEARCH_TOKEN", testCredential)
	leaking := map[string]string{
		providerBrave:   `{"web":{"results":[{"title":"t search-secret","url":"https://example.com/?k=search-secret","description":"d search-secret"}]}}`,
		providerTavily:  `{"results":[{"title":"t search-secret","url":"https://example.com/?k=search-secret","content":"d search-secret"}]}`,
		providerSearXNG: `{"results":[{"title":"t search-secret","url":"https://example.com/?k=search-secret","content":"d search-secret"}]}`,
	}
	for provider, body := range leaking {
		t.Run(provider, func(t *testing.T) {
			options := optionsFor(provider)
			options.CredentialRef = "env://AURA_TEST_SEARCH_TOKEN"
			options.client = cannedClient(http.StatusOK, body, nil)
			got, err := callSearch(newAdapter(t, options), `{"query":"go"}`)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if strings.Contains(string(got.Output), testCredential) {
				t.Fatalf("output leaks the credential: %s", got.Output)
			}
			results := decodeResults(t, got.Output)
			if len(results) != 1 || results[0].Title != "t [redacted]" || results[0].Snippet != "d [redacted]" || results[0].URL != "https://example.com/?k=[redacted]" {
				t.Fatalf("results = %+v", results)
			}
		})
	}
}

func TestArgumentsAreValidated(t *testing.T) {
	options := optionsFor(providerDuckDuckGo)
	options.client = cannedClient(http.StatusOK, duckDuckGoFixture, nil)
	adapter := newAdapter(t, options)
	for _, arguments := range []string{`{}`, `{"query":"  "}`, `not json`, `{"query":3}`} {
		if _, err := callSearch(adapter, arguments); classOf(err) != toolbroker.ResultInvalidArgument {
			t.Errorf("arguments %q: err = %v", arguments, err)
		}
	}
	if _, err := adapter(context.Background(), nil, approval.Constraints{}); classOf(err) != toolbroker.ResultInvalidArgument {
		t.Errorf("nil request: err = %v", err)
	}
}

func TestMaxResultsArgumentOnlyNarrowsTheConfiguredLimit(t *testing.T) {
	options := optionsFor(providerSearXNG)
	options.MaxResults = 1
	options.client = cannedClient(http.StatusOK, searxngFixture, nil)
	got, err := callSearch(newAdapter(t, options), `{"query":"go","max_results":10}`)
	if err != nil || len(decodeResults(t, got.Output)) != 1 {
		t.Fatalf("output = %s, err = %v", got.Output, err)
	}
}

func TestNewRejectsOptionsThatFallThroughToADefaultOrRedirectAKey(t *testing.T) {
	tests := map[string]func(*Options){
		"empty provider":              func(o *Options) { o.Provider = "" },
		"unknown provider":            func(o *Options) { o.Provider = "bing" },
		"upper case provider":         func(o *Options) { o.Provider = "Brave" },
		"duckduckgo endpoint":         func(o *Options) { o.Provider = providerDuckDuckGo; o.CredentialRef = ""; o.Endpoint = searxngEndpoint },
		"brave endpoint":              func(o *Options) { o.Provider = providerBrave; o.Endpoint = searxngEndpoint },
		"tavily endpoint":             func(o *Options) { o.Provider = providerTavily; o.Endpoint = searxngEndpoint },
		"brave loopback authority":    func(o *Options) { o.Provider = providerBrave; o.LoopbackAuthority = "127.0.0.1:8888" },
		"duckduckgo credential":       func(o *Options) { o.Provider = providerDuckDuckGo; o.CredentialRef = "env://X" },
		"searxng without endpoint":    func(o *Options) { o.Endpoint = "" },
		"searxng plain http":          func(o *Options) { o.Endpoint = "http://search.example/search" },
		"searxng credentials in url":  func(o *Options) { o.Endpoint = "https://user:pw@search.example/search" },
		"searxng query in url":        func(o *Options) { o.Endpoint = "https://search.example/search?x=1" },
		"loopback endpoint https":     func(o *Options) { o.Endpoint = "https://127.0.0.1:8888/search"; o.LoopbackAuthority = "127.0.0.1:8888" },
		"loopback authority mismatch": func(o *Options) { o.Endpoint = "http://127.0.0.1:8888/search"; o.LoopbackAuthority = "127.0.0.1:9999" },
		"loopback with query": func(o *Options) {
			o.Endpoint = "http://127.0.0.1:8888/search?x=1"
			o.LoopbackAuthority = "127.0.0.1:8888"
		},
		"non loopback authority": func(o *Options) { o.Endpoint = "http://10.0.0.5:8888/search"; o.LoopbackAuthority = "10.0.0.5:8888" },
		"localhost authority":    func(o *Options) { o.Endpoint = "http://localhost:8888/search"; o.LoopbackAuthority = "localhost:8888" },
		"zero timeout":           func(o *Options) { o.Timeout = 0 },
		"too many results":       func(o *Options) { o.MaxResults = 21 },
		"zero results":           func(o *Options) { o.MaxResults = 0 },
		"zero body limit":        func(o *Options) { o.MaxBodyBytes = 0 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			options := optionsFor(providerSearXNG)
			mutate(options)
			if _, err := New(options); err == nil {
				t.Fatalf("New accepted %+v", options)
			}
		})
	}
	if _, err := New(nil); err == nil {
		t.Fatal("New accepted nil options")
	}
}

func TestMediatedClientDeniesPrivateEndpointsBeforeDialingAndKeepsTheQueryOutOfTheError(t *testing.T) {
	const query = "needle-query-91c2"
	for _, host := range []string{"127.0.0.1", "10.0.0.5", "internal.example", "169.254.169.254"} {
		t.Run(host, func(t *testing.T) {
			resolver := &recordingResolver{staticResolver: searchResolver()}
			options := optionsFor(providerSearXNG)
			options.Endpoint = "https://" + host + "/search"
			options.Resolver = resolver
			_, err := callSearch(newAdapter(t, options), `{"query":"`+query+`"}`)
			if code, ok := egress.CodeOf(err); !ok || code != egress.ErrorCodeEgressDenied {
				t.Fatalf("err = %v, want egress_denied", err)
			}
			if strings.Contains(err.Error(), query) {
				t.Fatalf("the query reached the error: %v", err)
			}
		})
	}
}

func TestRemoteClientNeverFollowsRedirects(t *testing.T) {
	resolver := &recordingResolver{staticResolver: searchResolver()}
	options := optionsFor(providerSearXNG)
	options.Resolver = resolver
	client, err := buildClient(options)
	if err != nil {
		t.Fatalf("buildClient: %v", err)
	}
	next, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://search.example/elsewhere", http.NoBody)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	first, err := http.NewRequestWithContext(t.Context(), http.MethodGet, searxngEndpoint, http.NoBody)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	for _, target := range []string{"https://search.example/elsewhere", "https://internal.example/steal", "https://other.example/x"} {
		next.URL, _ = url.Parse(target)
		if err := client.CheckRedirect(next, []*http.Request{first}); !errors.Is(err, http.ErrUseLastResponse) {
			t.Fatalf("CheckRedirect(%q) = %v, want ErrUseLastResponse", target, err)
		}
	}
	if resolver.lookups() != 0 {
		t.Fatalf("resolver lookups = %d, want none", resolver.lookups())
	}
}

func TestTransportFailuresCarryNeitherTheQueryNorProviderText(t *testing.T) {
	const query = "needle-query-77d0"
	leak := errors.New("dial failed for https://search.example/search?q=" + query + " location provider-text-marker")
	options := optionsFor(providerSearXNG)
	options.client = failingClient(leak)
	_, err := callSearch(newAdapter(t, options), `{"query":"`+query+`"}`)
	if classOf(err) != toolbroker.ResultExecutionFailed || detailOf(err) != "search provider request failed" {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), query) || strings.Contains(err.Error(), "provider-text-marker") {
		t.Fatalf("error leaks request or provider text: %v", err)
	}
}

func TestDeadlineAndCancellationKeepTheirIdentity(t *testing.T) {
	for name, cause := range map[string]error{"deadline": context.DeadlineExceeded, "cancellation": context.Canceled} {
		t.Run(name, func(t *testing.T) {
			options := optionsFor(providerSearXNG)
			options.client = failingClient(cause)
			_, err := callSearch(newAdapter(t, options), `{"query":"go"}`)
			if !errors.Is(err, cause) {
				t.Fatalf("err = %v, want %v", err, cause)
			}
			var urlErr *url.Error
			if errors.As(err, &urlErr) {
				t.Fatalf("err still wraps the request URL: %v", err)
			}
		})
	}
}

func TestSlowProviderEndsWithTheContextDeadline(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer server.Close()
	defer close(release)
	options := loopbackOptions(server)
	options.Timeout = 50 * time.Millisecond
	_, err := callSearch(newAdapter(t, options), `{"query":"go"}`)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
}

func loopbackOptions(server *httptest.Server) *Options {
	options := optionsFor(providerSearXNG)
	options.Endpoint = server.URL + "/search"
	options.LoopbackAuthority = strings.TrimPrefix(server.URL, "http://")
	return options
}

func TestLoopbackClientReachesARealSearXNGServer(t *testing.T) {
	t.Setenv("AURA_TEST_SEARCH_TOKEN", testCredential)
	for name, ref := range map[string]string{"without credential": "", "with credential": "env://AURA_TEST_SEARCH_TOKEN"} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var seen *http.Request
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				seen = r.Clone(r.Context())
				mu.Unlock()
				_, _ = io.WriteString(w, searxngFixture)
			}))
			defer server.Close()
			options := loopbackOptions(server)
			options.CredentialRef = ref
			got, err := callSearch(newAdapter(t, options), `{"query":"go lang","max_results":2}`)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if len(decodeResults(t, got.Output)) != 2 {
				t.Fatalf("output = %s", got.Output)
			}
			mu.Lock()
			defer mu.Unlock()
			if seen == nil || seen.URL.Path != "/search" || seen.URL.Query().Get("q") != "go lang" || seen.URL.Query().Get("format") != "json" {
				t.Fatalf("request = %+v", seen)
			}
			authorization := seen.Header.Get("Authorization")
			if ref == "" && authorization != "" {
				t.Fatalf("Authorization = %q, want none", authorization)
			}
			if ref != "" && authorization != "Bearer "+testCredential {
				t.Fatalf("Authorization = %q", authorization)
			}
		})
	}
}

func TestLoopbackClientDoesNotFollowRedirects(t *testing.T) {
	var mu sync.Mutex
	followed := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		mu.Lock()
		followed++
		mu.Unlock()
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/stolen", http.StatusFound)
	}))
	defer server.Close()
	_, err := callSearch(newAdapter(t, loopbackOptions(server)), `{"query":"go"}`)
	if classOf(err) != toolbroker.ResultExecutionFailed || detailOf(err) != "search provider returned status 302" {
		t.Fatalf("err = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if followed != 0 {
		t.Fatalf("the redirect target was requested %d times", followed)
	}
}

func TestMalformedRedirectLocationNeverReachesTheError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://[::1/provider-text-marker")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	_, err := callSearch(newAdapter(t, loopbackOptions(server)), `{"query":"needle-query-5b1e"}`)
	if classOf(err) != toolbroker.ResultExecutionFailed || detailOf(err) != "search provider request failed" {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "provider-text-marker") || strings.Contains(err.Error(), "needle-query") {
		t.Fatalf("error leaks provider or request text: %v", err)
	}
}

func TestLoopbackClientRefusesAnythingButItsOwnAddress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	options := loopbackOptions(server)
	client, err := buildClient(options)
	if err != nil {
		t.Fatalf("buildClient: %v", err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:1/search?q=needle", http.NoBody)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	response, err := client.Do(request)
	if response != nil {
		_ = response.Body.Close()
	}
	if code, ok := egress.CodeOf(err); !ok || code != egress.ErrorCodeEgressDenied {
		t.Fatalf("err = %v, want egress_denied", err)
	}
}
