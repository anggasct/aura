package mcp

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

var errResponseTooLarge = errors.New("mcp: response exceeds the configured message bound")

const maxRedirectHops = 10

type cappedTransport struct {
	next  http.RoundTripper
	limit int64
}

func (t *cappedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, Errorf(ErrConfigInvalid, "request must not be nil")
	}
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, Errorf(ErrServerUnavailable, "empty response from server")
	}
	if t.limit > 0 && resp.ContentLength > t.limit {
		_ = resp.Body.Close()
		return nil, Errorf(ErrMessageTooLarge, "response exceeds the configured message bound")
	}
	if resp.Body != nil {
		resp.Body = &cappedReader{source: resp.Body, remaining: t.limit}
	}
	return resp, nil
}

type cappedReader struct {
	source    io.ReadCloser
	remaining int64
}

func (r *cappedReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, errResponseTooLarge
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.source.Read(p)
	r.remaining -= int64(n)
	if r.remaining <= 0 && err == nil {
		return n, errResponseTooLarge
	}
	return n, err
}

func (r *cappedReader) Close() error {
	return r.source.Close()
}

func (t *cappedTransport) CloseIdleConnections() {
	if closer, ok := t.next.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func isResponseTooLarge(err error) bool {
	return errors.Is(err, errResponseTooLarge)
}

func sameOrigin(first, second *url.URL) bool {
	if first == nil || second == nil {
		return false
	}
	return strings.EqualFold(first.Scheme, second.Scheme) && strings.EqualFold(first.Host, second.Host)
}

func checkRedirectPolicy(req *http.Request, via []*http.Request, allowRedirects bool, baseCheck func(*http.Request, []*http.Request) error) error {
	if len(via) == 0 {
		return nil
	}
	if !allowRedirects {
		return http.ErrUseLastResponse
	}
	if len(via) >= maxRedirectHops {
		return Errorf(ErrEgressDenied, "redirect chain exceeds the hop bound")
	}
	previous := via[len(via)-1].URL
	if previous == nil || req.URL == nil {
		return Errorf(ErrEgressDenied, "redirect target is not valid")
	}
	if !sameOrigin(previous, req.URL) {
		return Errorf(ErrEgressDenied, "cross-origin redirect denied")
	}
	if baseCheck != nil {
		return baseCheck(req, via)
	}
	return nil
}
