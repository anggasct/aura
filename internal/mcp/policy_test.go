package mcp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type stubRoundTripper struct {
	roundTrip func(req *http.Request) (*http.Response, error)
}

func (s *stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return s.roundTrip(req)
}

type stubEndpointPolicy struct {
	err error
}

func (s stubEndpointPolicy) ValidateEndpoint(_ context.Context, _ string) error {
	return s.err
}

func cannedResponse(body string, contentLength int64) *http.Response {
	return &http.Response{
		StatusCode:    http.StatusOK,
		Body:          io.NopCloser(strings.NewReader(body)),
		Header:        http.Header{},
		ContentLength: contentLength,
	}
}

func TestCappedTransportEnforcesBound(t *testing.T) {
	inner := &stubRoundTripper{roundTrip: func(_ *http.Request) (*http.Response, error) {
		resp := cannedResponse(strings.Repeat("x", 64), -1)
		return resp, nil
	}}
	transport := &cappedTransport{next: inner, limit: 16}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1/x", http.NoBody)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip(): %v", err)
	}
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("over-limit body accepted")
	} else if !isResponseTooLarge(err) {
		t.Fatalf("error = %v, want overflow", err)
	}
	_ = resp.Body.Close()
}

func TestCappedTransportRejectsDeclaredLength(t *testing.T) {
	closed := false
	inner := &stubRoundTripper{roundTrip: func(_ *http.Request) (*http.Response, error) {
		resp := cannedResponse("", 1<<20)
		original := resp.Body
		resp.Body = &closeTracker{ReadCloser: original, closed: &closed}
		return resp, nil
	}}
	transport := &cappedTransport{next: inner, limit: 16}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1/x", http.NoBody)
	resp, err := transport.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("declared over-limit body accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrMessageTooLarge {
		t.Fatalf("code = %v, %v", code, ok)
	}
	if !closed {
		t.Error("rejected body was not closed")
	}
}

func TestCappedTransportPassesSmallBodies(t *testing.T) {
	inner := &stubRoundTripper{roundTrip: func(_ *http.Request) (*http.Response, error) {
		return cannedResponse("ok", 2), nil
	}}
	transport := &cappedTransport{next: inner, limit: 16}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1/x", http.NoBody)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip(): %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || string(body) != "ok" {
		t.Errorf("body = %q, %v", body, err)
	}
}

func TestCappedTransportExactLimitAllowed(t *testing.T) {
	inner := &stubRoundTripper{roundTrip: func(_ *http.Request) (*http.Response, error) {
		return cannedResponse(strings.Repeat("x", 16), -1), nil
	}}
	transport := &cappedTransport{next: inner, limit: 16}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1/x", http.NoBody)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip(): %v", err)
	}
	var body []byte
	buf := make([]byte, 4)
	for {
		n, err := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = resp.Body.Close()
			t.Fatalf("Read(): %v", err)
		}
	}
	_ = resp.Body.Close()
	if len(body) != 16 {
		t.Fatalf("body length = %d, want 16", len(body))
	}
}

func TestCappedTransportLimitPlusOneRejected(t *testing.T) {
	inner := &stubRoundTripper{roundTrip: func(_ *http.Request) (*http.Response, error) {
		return cannedResponse(strings.Repeat("x", 17), -1), nil
	}}
	transport := &cappedTransport{next: inner, limit: 16}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1/x", http.NoBody)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip(): %v", err)
	}
	if _, err := io.ReadAll(resp.Body); err == nil {
		_ = resp.Body.Close()
		t.Fatal("limit-plus-one body accepted")
	} else if !isResponseTooLarge(err) {
		_ = resp.Body.Close()
		t.Fatalf("error = %v, want overflow", err)
	} else {
		_ = resp.Body.Close()
	}
}

type closeTracker struct {
	io.ReadCloser
	closed *bool
}

func (c *closeTracker) Close() error {
	*c.closed = true
	return c.ReadCloser.Close()
}

func TestRedirectPolicy(t *testing.T) {
	base := func(_ *http.Request, _ []*http.Request) error { return nil }
	origin, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/a", http.NoBody)
	same, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/b", http.NoBody)
	other, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://other.example/c", http.NoBody)

	if err := checkRedirectPolicy(same, nil, true, base); err != nil {
		t.Errorf("first request rejected: %v", err)
	}
	if err := checkRedirectPolicy(same, []*http.Request{origin}, false, base); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("disabled redirect = %v, want use-last-response", err)
	}
	if err := checkRedirectPolicy(other, []*http.Request{origin}, true, base); err == nil {
		t.Error("cross-origin redirect accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrEgressDenied {
		t.Errorf("code = %v, %v", code, ok)
	}
	if err := checkRedirectPolicy(same, []*http.Request{origin}, true, base); err != nil {
		t.Errorf("same-origin redirect rejected: %v", err)
	}
	hops := make([]*http.Request, maxRedirectHops)
	for i := range hops {
		hops[i] = origin
	}
	if err := checkRedirectPolicy(same, hops, true, base); err == nil {
		t.Error("over-long chain accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrEgressDenied {
		t.Errorf("code = %v, %v", code, ok)
	}
}
