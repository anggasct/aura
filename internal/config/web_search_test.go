package config

import (
	"testing"
)

func TestWebSearchProvidersAreEachAcceptedByValidation(t *testing.T) {
	for _, provider := range WebSearchProviders() {
		webSearch := ToolWebSearch{Provider: provider, CredentialRef: DefaultWebSearchCredentialRef(provider), MaxResults: 5}
		if provider == WebSearchProviderSearXNG {
			webSearch.Endpoint = "https://search.example.com/search"
		}
		if err := validateWebSearch(&webSearch); err != nil {
			t.Errorf("validateWebSearch(%s) = %v", provider, err)
		}
	}
}

func TestSearchEndpointOnNilReceiverIsInvalid(t *testing.T) {
	var webSearch *ToolWebSearch
	if _, err := webSearch.SearchEndpoint(); err == nil {
		t.Fatal("SearchEndpoint on nil succeeded")
	}
}
