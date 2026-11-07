package fetch

import (
	"context"
	"encoding/json"
	"errors"
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

func TestFetchLimitsRedirects(t *testing.T) {
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
	if _, err := adapter(context.Background(), fetchRequest("https://public.example/redirect"), constraints()); err == nil {
		t.Error("followed a redirect past the limit")
	}
}

func TestFetchSendsTheQueryAndDropsTheFragment(t *testing.T) {
	var sent *http.Request
	adapter, err := New(Options{
		Timeout:         time.Second,
		MaxEncodedBytes: 1024,
		MaxDecodedBytes: 1024,
		client: cannedClient(func(req *http.Request) (*http.Response, error) {
			sent = req
			return cannedResponse(req, "text/plain", "ok"), nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	output, err := adapter(context.Background(), fetchRequest("https://public.example/doc?a=1&b=two%20words#part"), constraints())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if sent.URL.Path != "/doc" || sent.URL.RawQuery != "a=1&b=two%20words" || sent.URL.Fragment != "" {
		t.Fatalf("sent URL = %q", sent.URL.String())
	}
	var body result
	if err := json.Unmarshal(output.Output, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.URL != "https://public.example/doc?a=1&b=two%20words" {
		t.Fatalf("result url = %q", body.URL)
	}
}

func TestFetchDeniesUnsafeDestinationsCarryingAQuery(t *testing.T) {
	adapter, err := New(Options{
		Timeout:         time.Second,
		MaxEncodedBytes: 1024,
		MaxDecodedBytes: 1024,
		Resolver:        publicResolverWithPrivate(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, raw := range []string{"https://127.0.0.1/doc?a=1", "https://10.0.0.5/doc?a=1", "https://internal.example/doc?a=1", "http://public.example/doc?a=1"} {
		_, err := adapter(context.Background(), fetchRequest(raw), constraints())
		if code, ok := egress.CodeOf(err); !ok || code != egress.ErrorCodeEgressDenied {
			t.Errorf("fetch(%q) = %v, want egress_denied before dialing", raw, err)
		}
	}
}

func TestFetchFollowsOnlySameOriginRedirectsToAQuery(t *testing.T) {
	for name, tc := range map[string]struct {
		location string
		followed bool
	}{
		"same origin":  {location: "https://public.example/final?a=1", followed: true},
		"other origin": {location: "https://other.example/final?a=1"},
	} {
		t.Run(name, func(t *testing.T) {
			client := egress.NewAPIClient(staticResolver{
				"public.example": {net.ParseIP("93.184.216.34")},
				"other.example":  {net.ParseIP("93.184.216.35")},
			})
			client.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/start" {
					return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{tc.location}}, Body: http.NoBody, Request: req}, nil
				}
				return cannedResponse(req, "text/plain", "ok"), nil
			})
			adapter, err := New(Options{Timeout: time.Second, MaxRedirects: 2, MaxEncodedBytes: 1024, MaxDecodedBytes: 1024, client: client})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = adapter(context.Background(), fetchRequest("https://public.example/start"), constraints())
			if tc.followed && err != nil {
				t.Fatalf("fetch: %v", err)
			}
			if code, ok := egress.CodeOf(err); !tc.followed && (!ok || code != egress.ErrorCodeEgressDenied) {
				t.Fatalf("fetch = %v, want egress_denied", err)
			}
		})
	}
}

func TestFetchTransportErrorsCarryNoURL(t *testing.T) {
	adapter, err := New(Options{
		Timeout:         time.Second,
		MaxEncodedBytes: 1024,
		MaxDecodedBytes: 1024,
		client: cannedClient(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = adapter(context.Background(), fetchRequest("https://public.example/doc?token=hunter2"), constraints())
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error = %v", err)
	}
}

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

func chunkedClient(contentType, body string) *http.Client {
	return cannedClient(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{contentType}},
			Body:          io.NopCloser(strings.NewReader(body)),
			ContentLength: -1,
			Request:       req,
		}, nil
	})
}

func TestFetchEnforcesEncodedLimitForChunkedResponses(t *testing.T) {
	adapter, err := New(Options{
		Timeout:         time.Second,
		MaxRedirects:    2,
		MaxEncodedBytes: 1024,
		MaxDecodedBytes: 8192,
		client:          chunkedClient("text/plain", strings.Repeat("a", 4096)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = adapter(context.Background(), fetchRequest("https://public.example/doc"), constraints())
	if class := classOf(err); class != toolbroker.ResultPolicyDenied {
		t.Fatalf("class = %q, err = %v, want policy_denied", class, err)
	}
	if !strings.Contains(err.Error(), "encoded") {
		t.Fatalf("error should name the encoded limit: %v", err)
	}
}

func TestFetchEnforcesEncodedLimitWhenDecodedCapIsLower(t *testing.T) {
	adapter, err := New(Options{
		Timeout:         time.Second,
		MaxRedirects:    2,
		MaxEncodedBytes: 1024,
		MaxDecodedBytes: 512,
		client:          chunkedClient("text/plain", strings.Repeat("a", 4096)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = adapter(context.Background(), fetchRequest("https://public.example/doc"), constraints())
	if class := classOf(err); class != toolbroker.ResultPolicyDenied {
		t.Fatalf("class = %q, err = %v, want policy_denied (encoded over-limit must not be truncated into acceptance)", class, err)
	}
	if !strings.Contains(err.Error(), "encoded") {
		t.Fatalf("error should name the encoded limit: %v", err)
	}
}

func TestFetchRejectsCompressedResponses(t *testing.T) {
	adapter, err := New(Options{
		Timeout:         time.Second,
		MaxRedirects:    2,
		MaxEncodedBytes: 1024,
		MaxDecodedBytes: 8192,
		client: cannedClient(func(req *http.Request) (*http.Response, error) {
			response := cannedResponse(req, "text/plain", "compressed payload")
			response.Header.Set("Content-Encoding", "gzip")
			return response, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = adapter(context.Background(), fetchRequest("https://public.example/doc"), constraints())
	if class := classOf(err); class != toolbroker.ResultPolicyDenied {
		t.Fatalf("class = %q, err = %v, want policy_denied", class, err)
	}
}

func TestFetchAcceptsTransparentlyDecompressedBody(t *testing.T) {
	adapter, err := New(Options{
		Timeout:         time.Second,
		MaxRedirects:    2,
		MaxEncodedBytes: 1024,
		MaxDecodedBytes: 8192,
		client: cannedClient(func(req *http.Request) (*http.Response, error) {
			response := cannedResponse(req, "text/plain", strings.Repeat("a", 2048))
			response.Uncompressed = true
			return response, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	outcome, err := adapter(context.Background(), fetchRequest("https://public.example/doc"), constraints())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	var body result
	if err := json.Unmarshal(outcome.Output, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Body) != 2048 || body.Truncated {
		t.Fatalf("body length = %d, truncated = %v", len(body.Body), body.Truncated)
	}
}

func TestFetchConvertsHTMLToMarkdown(t *testing.T) {
	document := `<html><head><style>body{color:red}</style><title>ignore</title></head><body>` +
		`<h1>Title</h1><p>Hello <b>world</b> and <i>farewell</i>.</p>` +
		`<a href="https://example.com/page">a link</a>` +
		`<script>alert("x")</script><style>h1{}</style>` +
		`<pre>line one
line two</pre></body></html>`
	adapter, err := New(Options{
		Timeout:         time.Second,
		MaxRedirects:    2,
		MaxEncodedBytes: 8192,
		MaxDecodedBytes: 8192,
		client:          chunkedClient("text/html", document),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	outcome, err := adapter(context.Background(), fetchRequest("https://public.example/doc"), constraints())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	var body result
	if err := json.Unmarshal(outcome.Output, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, want := range []string{"# Title", "**world**", "*farewell*", "[a link](https://example.com/page)", "```", "line two"} {
		if !strings.Contains(body.Body, want) {
			t.Errorf("converted body missing %q:\n%s", want, body.Body)
		}
	}
	for _, forbidden := range []string{"<h1>", "<b>", "<script>", "alert", "color:red", "<style>", "ignore"} {
		if strings.Contains(body.Body, forbidden) {
			t.Errorf("converted body still contains %q:\n%s", forbidden, body.Body)
		}
	}
}

func TestFetchLeavesNonHTMLBodiesUnchanged(t *testing.T) {
	for _, contentType := range []string{"text/plain", "application/json"} {
		adapter, err := New(Options{
			Timeout:         time.Second,
			MaxRedirects:    2,
			MaxEncodedBytes: 8192,
			MaxDecodedBytes: 8192,
			client:          chunkedClient(contentType, `{"a": 1}`),
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		outcome, err := adapter(context.Background(), fetchRequest("https://public.example/doc"), constraints())
		if err != nil {
			t.Fatalf("fetch(%s): %v", contentType, err)
		}
		var body result
		if err := json.Unmarshal(outcome.Output, &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Body != `{"a": 1}` {
			t.Fatalf("body = %q, want unchanged non-HTML content", body.Body)
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

func fetchRequest(raw string) *toolbroker.ToolRequest {
	return &toolbroker.ToolRequest{ToolName: "web_fetch", ToolVersion: "v1", Arguments: []byte(`{"url":"` + strings.ReplaceAll(raw, `"`, `\"`) + `"}`)}
}

func constraints() approval.Constraints { return approval.Constraints{} }

func classOf(err error) toolbroker.ResultClass {
	class, _ := toolbroker.CodeOf(err)
	return class
}
