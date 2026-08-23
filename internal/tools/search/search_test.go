package search

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
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

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

const providerBody = `{"web":{"results":[{"title":"title","url":"https://example.com","description":"search-secret"},{"title":"second","url":"https://example.org","description":"second"}]}}`

func cannedClient(captured *http.Request) *http.Client {
	return &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if captured != nil {
			*captured = *req
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(providerBody)),
			Request:    req,
		}, nil
	})}
}

func searchResolver() staticResolver {
	return staticResolver{
		"search.example":   {net.ParseIP("93.184.216.34")},
		"internal.example": {net.ParseIP("10.0.0.5")},
	}
}

func TestSearchUsesSecretReferenceAndBoundsResults(t *testing.T) {
	t.Setenv("AURA_TEST_SEARCH_TOKEN", "search-secret")
	var captured http.Request
	adapter, err := New(&Options{
		Provider:      "brave",
		CredentialRef: "env://AURA_TEST_SEARCH_TOKEN",
		Endpoint:      "https://search.example/res/v1/web/search",
		Timeout:       time.Second,
		MaxResults:    2,
		MaxBodyBytes:  4096,
		Resolver:      searchResolver(),
		client:        cannedClient(&captured),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	responseResult, err := adapter(context.Background(), &toolbroker.ToolRequest{ToolName: "web_search", ToolVersion: "v1", Arguments: []byte(`{"query":"aura","max_results":1}`)}, approval.Constraints{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var decoded result
	if err := json.Unmarshal(responseResult.Output, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Results) != 1 || decoded.Results[0].Title != "title" || decoded.Results[0].Snippet != "[redacted]" {
		t.Fatalf("result = %+v", decoded)
	}
	if captured.URL.String() != "https://search.example/res/v1/web/search" {
		t.Fatalf("provider request URL = %q, want the configured endpoint", captured.URL.String())
	}
	if captured.Header.Get("X-Subscription-Token") != "search-secret" {
		t.Fatal("provider credential header missing")
	}
}

func TestSearchMissingCredentialIsUnavailable(t *testing.T) {
	if err := os.Unsetenv("AURA_MISSING_SEARCH_TOKEN"); err != nil {
		t.Fatalf("Unsetenv: %v", err)
	}
	adapter, err := New(&Options{Provider: "brave", CredentialRef: "env://AURA_MISSING_SEARCH_TOKEN", Endpoint: "https://search.example/res/v1/web/search", Timeout: time.Second, MaxResults: 1, MaxBodyBytes: 1024, Resolver: searchResolver(), client: cannedClient(nil)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = adapter(context.Background(), &toolbroker.ToolRequest{ToolName: "web_search", ToolVersion: "v1", Arguments: []byte(`{"query":"aura"}`)}, approval.Constraints{})
	if class := classOf(err); class != toolbroker.ResultCapabilityUnavailable {
		t.Fatalf("class = %q, err = %v", class, err)
	}
}

func TestSearchRejectsUnsafeEndpointsBeforeDialing(t *testing.T) {
	t.Setenv("AURA_TEST_SEARCH_TOKEN", "search-secret")
	for _, endpoint := range []string{
		"http://search.example/res/v1/web/search",
		"https://user:pass@search.example/res/v1/web/search",
		"https://search.example/res/v1/web/search?extra=1",
	} {
		if _, err := New(&Options{
			Provider:      "brave",
			CredentialRef: "env://AURA_TEST_SEARCH_TOKEN",
			Endpoint:      endpoint,
			Timeout:       time.Second,
			MaxResults:    1,
			MaxBodyBytes:  4096,
			Resolver:      searchResolver(),
		}); err == nil {
			t.Errorf("endpoint %q was accepted at construction", endpoint)
		}
	}
}

// The production construction path has no client or transport injection:
// a provider endpoint that resolves to a private address must be denied by
// the mediated client before any connection is dialed.
func TestSearchMediatedClientDeniesPrivateEndpoints(t *testing.T) {
	t.Setenv("AURA_TEST_SEARCH_TOKEN", "search-secret")
	for _, endpoint := range []string{
		"https://127.0.0.1/res/v1/web/search",
		"https://10.0.0.5/res/v1/web/search",
		"https://internal.example/res/v1/web/search",
	} {
		adapter, err := New(&Options{
			Provider:      "brave",
			CredentialRef: "env://AURA_TEST_SEARCH_TOKEN",
			Endpoint:      endpoint,
			Timeout:       time.Second,
			MaxResults:    1,
			MaxBodyBytes:  4096,
			Resolver:      searchResolver(),
		})
		if err != nil {
			t.Fatalf("New(%q): %v", endpoint, err)
		}
		_, err = adapter(context.Background(), &toolbroker.ToolRequest{ToolName: "web_search", ToolVersion: "v1", Arguments: []byte(`{"query":"aura"}`)}, approval.Constraints{})
		if code, ok := egress.CodeOf(err); !ok || code != egress.ErrorCodeEgressDenied {
			t.Errorf("search(%q) = %v, want egress_denied before dialing", endpoint, err)
		}
	}
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	for _, options := range []*Options{{Provider: "", Timeout: time.Second, MaxResults: 1, MaxBodyBytes: 1}, {Provider: "brave", Timeout: 0, MaxResults: 1, MaxBodyBytes: 1}, {Provider: "brave", Timeout: time.Second, MaxResults: 21, MaxBodyBytes: 1}} {
		if _, err := New(options); err == nil {
			t.Errorf("New(%+v) accepted invalid options", options)
		}
	}
}

func classOf(err error) toolbroker.ResultClass {
	class, _ := toolbroker.CodeOf(err)
	return class
}
