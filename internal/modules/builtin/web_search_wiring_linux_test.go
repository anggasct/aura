//go:build linux

package toolsbuiltin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

func TestEveryConfiguredWebSearchProviderBuildsAnAdapter(t *testing.T) {
	for _, provider := range config.WebSearchProviders() {
		t.Run(provider, func(t *testing.T) {
			cfg := config.Default()
			cfg.Tools.Workspace = t.TempDir()
			cfg.Tools.WebSearch = config.ToolWebSearch{
				Provider:      provider,
				CredentialRef: config.DefaultWebSearchCredentialRef(provider),
				MaxResults:    5,
			}
			if provider == config.WebSearchProviderSearXNG {
				cfg.Tools.WebSearch.Endpoint = "https://search.example.com/search"
			}
			adapters, err := builtinAdapters(cfg.Tools, &handoffHolder{}, &emailHolder{}, &workflowHolder{}, &knowledgeHolder{}, &codeHolder{})
			if err != nil {
				t.Fatalf("builtinAdapters: %v", err)
			}
			if adapters["web_search@v1"] == nil {
				t.Fatal("web_search has no adapter")
			}
		})
	}
}

func TestLoadedSearXNGConfigReachesALoopbackServerThroughTheExecutorAdapters(t *testing.T) {
	t.Setenv("AURA_TEST_SEARXNG_TOKEN", "searxng-bearer-value")
	var mu sync.Mutex
	var seen *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = r.Clone(r.Context())
		mu.Unlock()
		_, _ = io.WriteString(w, `{"results":[{"title":"Go","url":"https://go.dev","content":"searxng-bearer-value"}]}`)
	}))
	defer server.Close()
	workspace := t.TempDir()
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "version: 1\ntools:\n  workspace: " + workspace + "\n  web_search:\n    provider: searxng\n    endpoint: " + server.URL + "/search\n    credential_ref: env://AURA_TEST_SEARXNG_TOKEN\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	adapters, err := builtinAdapters(loaded.Config.Tools, &handoffHolder{}, &emailHolder{}, &workflowHolder{}, &knowledgeHolder{}, &codeHolder{})
	if err != nil {
		t.Fatalf("builtinAdapters: %v", err)
	}
	got, err := adapters["web_search@v1"](context.Background(), &toolbroker.ToolRequest{ToolName: "web_search", ToolVersion: "v1", Arguments: json.RawMessage(`{"query":"go"}`)}, approval.Constraints{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var decoded struct {
		Results []struct {
			Title   string `json:"title"`
			Snippet string `json:"snippet"`
		} `json:"results"`
	}
	if err := json.Unmarshal(got.Output, &decoded); err != nil || len(decoded.Results) != 1 || decoded.Results[0].Title != "Go" || decoded.Results[0].Snippet != "[redacted]" {
		t.Fatalf("output = %s, err = %v", got.Output, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen == nil || seen.URL.Path != "/search" || seen.URL.Query().Get("q") != "go" || seen.Header.Get("Authorization") != "Bearer searxng-bearer-value" {
		t.Fatalf("request = %+v", seen)
	}
}

func TestExecutorRejectsASearchEndpointItCannotParse(t *testing.T) {
	cfg := config.Default()
	cfg.Tools.Workspace = t.TempDir()
	cfg.Tools.WebSearch = config.ToolWebSearch{Provider: config.WebSearchProviderSearXNG, Endpoint: "http://localhost:8888/search", MaxResults: 5}
	if _, err := builtinAdapters(cfg.Tools, &handoffHolder{}, &emailHolder{}, &workflowHolder{}, &knowledgeHolder{}, &codeHolder{}); err == nil {
		t.Fatal("builtinAdapters accepted a loopback name endpoint")
	}
}
