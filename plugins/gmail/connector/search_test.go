package connector

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/modules/plugin/wire"
)

func searchAPI(count int, missing map[string]bool) *fakeAPI {
	return newAPI(func(req *wire.Request) (*wire.Response, error) {
		path := strings.SplitN(strings.TrimPrefix(req.URL, apiBase), "?", 2)[0]
		if path == "messages" {
			refs := make([]map[string]string, 0, count)
			for i := range count {
				refs = append(refs, map[string]string{"id": fmt.Sprintf("m%d", i), "threadId": fmt.Sprintf("t%d", i)})
			}
			if count == 0 {
				return jsonResponse(200, map[string]any{"resultSizeEstimate": 0}), nil
			}
			return jsonResponse(200, map[string]any{"messages": refs, "resultSizeEstimate": count}), nil
		}
		id := strings.TrimPrefix(path, "messages/")
		if missing[id] {
			return statusOnly(404), nil
		}
		message := messageJSON(id, map[string]any{"headers": headersOf("From", "Alice <alice@example.com>", "Subject", "Subject "+id, "Date", "Tue, 3 Nov 2026 10:00:00 +0000", "To", "never@example.com")})
		message["snippet"] = "It&#39;s a snippet for " + id
		return jsonResponse(200, message), nil
	})
}

func searchResults(t *testing.T, api *fakeAPI, arguments any) searchOutput {
	t.Helper()
	raw, err := invoke(t, api, "search", arguments)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var decoded searchOutput
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return decoded
}

func TestSearchListsThenFetchesMetadataWithGetOnly(t *testing.T) {
	api := searchAPI(3, nil)
	got := searchResults(t, api, map[string]any{"query": "from:alice newer_than:7d", "max_results": 3})
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %+v", got.Messages)
	}
	first := got.Messages[0]
	want := summary{ID: "m0", ThreadID: "tm0", From: "Alice <alice@example.com>", Subject: "Subject m0", Date: "Tue, 3 Nov 2026 10:00:00 +0000", Snippet: "It's a snippet for m0"}
	if first != want {
		t.Fatalf("first = %+v, want %+v", first, want)
	}
	requests := api.seen()
	if len(requests) != 4 {
		t.Fatalf("requests = %d, want one list and three gets", len(requests))
	}
	for _, req := range requests {
		if req.Method != http.MethodGet {
			t.Fatalf("a %s request was made", req.Method)
		}
	}
	list := queryOf(t, requests[0].URL)
	if list.Get("q") != "from:alice newer_than:7d" || list.Get("maxResults") != "3" {
		t.Fatalf("list query = %v", list)
	}
	get := queryOf(t, requests[1].URL)
	if get.Get("format") != "metadata" || strings.Join(get["metadataHeaders"], ",") != "From,Subject,Date" {
		t.Fatalf("get query = %v", get)
	}
}

func TestSearchAtTheLimitStaysWithinTheStepBound(t *testing.T) {
	api := searchAPI(maxResults, nil)
	got := searchResults(t, api, map[string]any{"query": "in:inbox", "max_results": maxResults})
	if len(got.Messages) != maxResults || len(api.seen()) != wire.MaxSteps {
		t.Fatalf("messages = %d, requests = %d, want %d", len(got.Messages), len(api.seen()), wire.MaxSteps)
	}
}

func TestSearchDefaultsAndEmptyResults(t *testing.T) {
	api := searchAPI(0, nil)
	raw, err := invoke(t, api, "search", map[string]any{"query": ""})
	if err != nil || string(raw) != `{"messages":[]}` {
		t.Fatalf("output = %s, %v", raw, err)
	}
	list := queryOf(t, api.seen()[0].URL)
	if list.Get("maxResults") != "10" || list.Has("q") {
		t.Fatalf("list query = %v", list)
	}
}

func TestSearchSkipsAMessageDeletedBetweenListAndGet(t *testing.T) {
	api := searchAPI(3, map[string]bool{"m1": true})
	got := searchResults(t, api, map[string]any{"query": "x", "max_results": 3})
	if len(got.Messages) != 2 || got.Messages[0].ID != "m0" || got.Messages[1].ID != "m2" {
		t.Fatalf("messages = %+v", got.Messages)
	}
}

func TestSearchRejectsBadArgumentsBeforeAnyRequest(t *testing.T) {
	cases := []struct {
		name      string
		arguments any
	}{
		{"zero results", map[string]any{"query": "x", "max_results": 0}},
		{"too many results", map[string]any{"query": "x", "max_results": maxResults + 1}},
		{"negative results", map[string]any{"query": "x", "max_results": -1}},
		{"fractional results", json.RawMessage(`{"query":"x","max_results":2.5}`)},
		{"string results", map[string]any{"query": "x", "max_results": "3"}},
		{"query too long", map[string]any{"query": strings.Repeat("a", maxQueryBytes+1)}},
		{"missing arguments", json.RawMessage(`{}`)},
		{"unknown field", map[string]any{"query": "x", "page_token": "abc"}},
		{"case variant", json.RawMessage(`{"Query":"x"}`)},
		{"query of the wrong type", map[string]any{"query": 5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := searchAPI(1, nil)
			_, err := invoke(t, api, "search", tc.arguments)
			mustCode(t, err, CodeInvalidArgument)
			if len(api.seen()) != 0 {
				t.Fatalf("requests = %+v", api.seen())
			}
		})
	}
}

func TestSearchRefusesIDsFromGmailThatCouldReachOtherPaths(t *testing.T) {
	for _, id := range []string{"../drafts", "a/b", "m1?format=raw", ""} {
		t.Run("id "+id, func(t *testing.T) {
			api := newAPI(func(*wire.Request) (*wire.Response, error) {
				return jsonResponse(200, map[string]any{"messages": []map[string]string{{"id": id, "threadId": "t1"}}}), nil
			})
			_, err := invoke(t, api, "search", map[string]any{"query": "x"})
			mustCode(t, err, CodeResponseInvalid)
			if len(api.seen()) != 1 {
				t.Fatalf("requests = %+v", api.seen())
			}
		})
	}
}

func TestSearchRefusesMoreResultsThanAsked(t *testing.T) {
	api := searchAPI(5, nil)
	_, err := invoke(t, api, "search", map[string]any{"query": "x", "max_results": 2})
	mustCode(t, err, CodeResponseInvalid)
}

func TestSearchOutputNeverCarriesPlantedErrorText(t *testing.T) {
	api := newAPI(func(req *wire.Request) (*wire.Response, error) {
		if strings.Contains(req.URL, "format=metadata") {
			return statusOnly(500), nil
		}
		return jsonResponse(200, map[string]any{"messages": []map[string]string{{"id": "m1", "threadId": "t1"}}}), nil
	})
	raw, err := invoke(t, api, "search", map[string]any{"query": "x"})
	if err == nil {
		t.Fatalf("output = %s, want a failure", raw)
	}
	mustCode(t, err, CodeUpstream)
	if strings.Contains(string(raw), "PLANTED") {
		t.Fatalf("output carries provider text: %s", raw)
	}
}

func TestSearchSanitizesHeadersFromMail(t *testing.T) {
	api := newAPI(func(req *wire.Request) (*wire.Response, error) {
		if strings.HasPrefix(strings.TrimPrefix(req.URL, apiBase), "messages?") {
			return jsonResponse(200, map[string]any{"messages": []map[string]string{{"id": "m1", "threadId": "t1"}}}), nil
		}
		message := messageJSON("m1", map[string]any{"headers": headersOf("From", "x\r\n\x00 <x@example.com>", "Subject", "=?UTF-8?B?4pyT?= done", "Date", strings.Repeat("d", 5000))})
		message["snippet"] = "line\nbreak \x1b[0m"
		return jsonResponse(200, message), nil
	})
	got := searchResults(t, api, map[string]any{"query": "x"})
	entry := got.Messages[0]
	if entry.From != "x <x@example.com>" || entry.Subject != "✓ done" || len(entry.Date) != maxHeaderRunes || entry.Snippet != "line break [0m" {
		t.Fatalf("entry = %+v", entry)
	}
}
