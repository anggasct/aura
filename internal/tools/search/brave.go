package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
)

const braveEndpoint = "https://api.search.brave.com/res/v1/web/search"

type brave struct{}

type braveResponse struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"results"`
	} `json:"web"`
}

func (brave) newRequest(ctx context.Context, query string, limit int, credential string) (*http.Request, error) {
	values := url.Values{}
	values.Set("q", query)
	values.Set("count", strconv.Itoa(limit))
	request, err := newProviderRequest(ctx, http.MethodGet, braveEndpoint+"?"+values.Encode(), http.NoBody, "application/json")
	if err != nil {
		return nil, err
	}
	request.Header.Set("X-Subscription-Token", credential)
	return request, nil
}

func (brave) parse(body []byte) ([]searchResult, error) {
	var decoded braveResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, unreadableResponse()
	}
	results := make([]searchResult, 0, len(decoded.Web.Results))
	for _, item := range decoded.Web.Results {
		results = append(results, searchResult{Title: item.Title, URL: item.URL, Snippet: item.Description})
	}
	return results, nil
}

