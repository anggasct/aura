package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

const tavilyEndpoint = "https://api.tavily.com/search"

type tavily struct{}

type tavilyRequest struct {
	Query       string `json:"query"`
	MaxResults  int    `json:"max_results"`
	SearchDepth string `json:"search_depth"`
}

type tavilyResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

func (tavily) newRequest(ctx context.Context, query string, limit int, credential string) (*http.Request, error) {
	payload, err := json.Marshal(tavilyRequest{Query: query, MaxResults: limit, SearchDepth: "basic"})
	if err != nil {
		return nil, fmt.Errorf("search: encode request: %w", err)
	}
	request, err := newProviderRequest(ctx, http.MethodPost, tavilyEndpoint, bytes.NewReader(payload), "application/json")
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+credential)
	return request, nil
}

func (tavily) parse(body []byte) ([]searchResult, error) {
	var decoded tavilyResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, unreadableResponse()
	}
	results := make([]searchResult, 0, len(decoded.Results))
	for _, item := range decoded.Results {
		results = append(results, searchResult{Title: item.Title, URL: item.URL, Snippet: item.Content})
	}
	return results, nil
}
