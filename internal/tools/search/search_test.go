package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/toolbroker"
)

func TestSearchUsesSecretReferenceAndBoundsResults(t *testing.T) {
	t.Setenv("AURA_TEST_SEARCH_TOKEN", "search-secret")
	server := httptest.NewServer(http.HandlerFunc(searchHandler))
	defer server.Close()
	adapter, err := New(&Options{Provider: "brave", CredentialRef: "env://AURA_TEST_SEARCH_TOKEN", Endpoint: server.URL, Timeout: time.Second, MaxResults: 2, MaxBodyBytes: 4096, Client: server.Client()})
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
}

func TestSearchMissingCredentialIsUnavailable(t *testing.T) {
	if err := os.Unsetenv("AURA_MISSING_SEARCH_TOKEN"); err != nil {
		t.Fatalf("Unsetenv: %v", err)
	}
	adapter, err := New(&Options{Provider: "brave", CredentialRef: "env://AURA_MISSING_SEARCH_TOKEN", Endpoint: "http://127.0.0.1", Timeout: time.Second, MaxResults: 1, MaxBodyBytes: 1024, Client: &http.Client{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = adapter(context.Background(), &toolbroker.ToolRequest{ToolName: "web_search", ToolVersion: "v1", Arguments: []byte(`{"query":"aura"}`)}, approval.Constraints{})
	if class := classOf(err); class != toolbroker.ResultCapabilityUnavailable {
		t.Fatalf("class = %q, err = %v", class, err)
	}
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	for _, options := range []*Options{{Provider: "", Timeout: time.Second, MaxResults: 1, MaxBodyBytes: 1}, {Provider: "brave", Timeout: 0, MaxResults: 1, MaxBodyBytes: 1}, {Provider: "brave", Timeout: time.Second, MaxResults: 21, MaxBodyBytes: 1}} {
		if _, err := New(options); err == nil {
			t.Errorf("New(%+v) accepted invalid options", options)
		}
	}
}

func searchHandler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Subscription-Token") != "search-secret" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"web":{"results":[{"title":"title","url":"https://example.com","description":"search-secret"},{"title":"second","url":"https://example.org","description":"second"}]}}`))
}

func classOf(err error) toolbroker.ResultClass {
	class, _ := toolbroker.CodeOf(err)
	return class
}
