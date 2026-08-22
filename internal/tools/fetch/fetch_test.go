package fetch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/toolbroker"
)

func TestFetchBoundsBodyAndRejectsBinaryContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(contentHandler))
	defer server.Close()
	adapter, err := New(Options{Timeout: time.Second, MaxRedirects: 2, MaxEncodedBytes: 1024, MaxDecodedBytes: 3, Client: server.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	responseResult, err := adapter(context.Background(), fetchRequest(server.URL), constraints())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	var body result
	if err := json.Unmarshal(responseResult.Output, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Body != "abc" || !body.Truncated || body.StatusCode != http.StatusOK {
		t.Fatalf("result = %+v", body)
	}
	_, err = adapter(context.Background(), fetchRequest(server.URL+"/binary"), constraints())
	if class := classOf(err); class != toolbroker.ResultPolicyDenied {
		t.Fatalf("binary class = %q, err = %v", class, err)
	}
}

func TestFetchRejectsQueryAndLimitsRedirects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(redirectHandler))
	defer server.Close()
	adapter, err := New(Options{Timeout: time.Second, MaxRedirects: 0, MaxEncodedBytes: 1024, MaxDecodedBytes: 1024, Client: server.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, raw := range []string{server.URL + "?secret=1", server.URL + "/redirect"} {
		_, err := adapter(context.Background(), fetchRequest(raw), constraints())
		if err == nil {
			t.Errorf("accepted unsafe URL %q", raw)
		}
	}
}

func TestNewRejectsInvalidLimits(t *testing.T) {
	for _, options := range []Options{
		{Timeout: 0, MaxRedirects: 1, MaxEncodedBytes: 1, MaxDecodedBytes: 1},
		{Timeout: time.Second, MaxRedirects: 11, MaxEncodedBytes: 1, MaxDecodedBytes: 1},
		{Timeout: time.Second, MaxRedirects: 1, MaxEncodedBytes: 0, MaxDecodedBytes: 1},
	} {
		if _, err := New(options); err == nil {
			t.Errorf("New(%+v) accepted invalid options", options)
		}
	}
}

func contentHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/binary" {
		w.Header().Set("Content-Type", "application/octet-stream")
	} else {
		w.Header().Set("Content-Type", "text/plain")
	}
	_, _ = w.Write([]byte("abcdef"))
}

func redirectHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/redirect" {
		http.Redirect(w, r, "/final", http.StatusFound)
		return
	}
	_, _ = w.Write([]byte("ok"))
}

func fetchRequest(raw string) *toolbroker.ToolRequest {
	return &toolbroker.ToolRequest{ToolName: "web_fetch", ToolVersion: "v1", Arguments: []byte(`{"url":"` + strings.ReplaceAll(raw, `"`, `\"`) + `"}`)}
}

func constraints() approval.Constraints { return approval.Constraints{} }

func classOf(err error) toolbroker.ResultClass {
	class, _ := toolbroker.CodeOf(err)
	return class
}
