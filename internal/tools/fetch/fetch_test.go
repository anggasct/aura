package fetch

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
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

func cannedClient(handle func(*http.Request) (*http.Response, error)) *http.Client {
	return &http.Client{Transport: roundTripperFunc(handle)}
}

func cannedResponse(req *http.Request, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func TestFetchBoundsBodyAndRejectsBinaryContent(t *testing.T) {
	adapter, err := New(Options{
		Timeout:         time.Second,
		MaxRedirects:    2,
		MaxEncodedBytes: 1024,
		MaxDecodedBytes: 3,
		client: cannedClient(func(req *http.Request) (*http.Response, error) {
			contentType := "text/plain"
			if req.URL.Path == "/binary" {
				contentType = "application/octet-stream"
			}
			return cannedResponse(req, contentType, "abcdef"), nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	responseResult, err := adapter(context.Background(), fetchRequest("https://public.example/doc"), constraints())
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
	_, err = adapter(context.Background(), fetchRequest("https://public.example/binary"), constraints())
	if class := classOf(err); class != toolbroker.ResultPolicyDenied {
		t.Fatalf("binary class = %q, err = %v", class, err)
	}
}

func TestFetchRejectsQueryAndLimitsRedirects(t *testing.T) {
	adapter, err := New(Options{
		Timeout:         time.Second,
		MaxRedirects:    0,
		MaxEncodedBytes: 1024,
		MaxDecodedBytes: 1024,
		client: cannedClient(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == "/redirect" {
				return &http.Response{
					StatusCode: http.StatusFound,
					Header:     http.Header{"Location": []string{"https://public.example/final"}},
					Body:       http.NoBody,
					Request:    req,
				}, nil
			}
			return cannedResponse(req, "text/plain", "ok"), nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, raw := range []string{"https://public.example/doc?secret=1", "https://public.example/redirect"} {
		_, err := adapter(context.Background(), fetchRequest(raw), constraints())
		if err == nil {
			t.Errorf("accepted unsafe URL %q", raw)
		}
	}
}

// The production construction path has no client or transport injection:
// unsafe destinations must be denied by the mediated client before any
// connection is dialed.
func TestFetchMediatedClientDeniesUnsafeDestinations(t *testing.T) {
	resolutions := 0
	counting := resolverFunc(func(ctx context.Context, host string) ([]net.IP, error) {
		resolutions++
		return publicResolverWithPrivate().LookupIP(ctx, host)
	})
	adapter, err := New(Options{
		Timeout:         time.Second,
		MaxRedirects:    2,
		MaxEncodedBytes: 1024,
		MaxDecodedBytes: 1024,
		Resolver:        counting,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, raw := range []string{
		"https://127.0.0.1/doc",
		"https://10.0.0.5/doc",
		"https://internal.example/doc",
		"http://public.example/doc",
	} {
		_, err := adapter(context.Background(), fetchRequest(raw), constraints())
		if code, ok := egress.CodeOf(err); !ok || code != egress.ErrorCodeEgressDenied {
			t.Errorf("fetch(%q) = %v, want egress_denied before dialing", raw, err)
		}
	}
	if resolutions != 1 {
		t.Fatalf("resolver calls = %d, want 1 (internal.example resolves private and is rejected; literals never resolve)", resolutions)
	}
}

func publicResolverWithPrivate() staticResolver {
	return staticResolver{
		"internal.example": {net.ParseIP("10.0.0.5")},
		"public.example":   {net.ParseIP("93.184.216.34")},
	}
}

type resolverFunc func(context.Context, string) ([]net.IP, error)

func (f resolverFunc) LookupIP(ctx context.Context, host string) ([]net.IP, error) {
	return f(ctx, host)
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

func fetchRequest(raw string) *toolbroker.ToolRequest {
	return &toolbroker.ToolRequest{ToolName: "web_fetch", ToolVersion: "v1", Arguments: []byte(`{"url":"` + strings.ReplaceAll(raw, `"`, `\"`) + `"}`)}
}

func constraints() approval.Constraints { return approval.Constraints{} }

func classOf(err error) toolbroker.ResultClass {
	class, _ := toolbroker.CodeOf(err)
	return class
}
