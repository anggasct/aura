package search

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/anggasct/aura/internal/toolbroker"
)

type searXNG struct {
	endpoint string
}

type searXNGResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
	UnresponsiveEngines []json.RawMessage `json:"unresponsive_engines"`
}

func (s searXNG) newRequest(ctx context.Context, query string, _ int, credential string) (*http.Request, error) {
	target, err := url.Parse(s.endpoint)
	if err != nil {
		return nil, fmt.Errorf("search: parse endpoint: %w", err)
	}
	values := url.Values{}
	values.Set("q", query)
	values.Set("format", "json")
	target.RawQuery = values.Encode()
	request, err := newProviderRequest(ctx, http.MethodGet, target.String(), http.NoBody, "application/json")
	if err != nil {
		return nil, err
	}
	if credential != "" {
		request.Header.Set("Authorization", "Bearer "+credential)
	}
	return request, nil
}

func (searXNG) parse(body []byte) ([]searchResult, error) {
	var decoded searXNGResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, unreadableResponse()
	}
	if len(decoded.Results) == 0 && len(decoded.UnresponsiveEngines) > 0 {
		return nil, toolbroker.Errorf(toolbroker.ResultExecutionFailed, detailEnginesFailed)
	}
	results := make([]searchResult, 0, len(decoded.Results))
	for _, item := range decoded.Results {
		results = append(results, searchResult{Title: item.Title, URL: item.URL, Snippet: item.Content})
	}
	return results, nil
}
