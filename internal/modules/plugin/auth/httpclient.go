package auth

import (
	"errors"
	"io"
	"net/http"
	"time"
)

const (
	maxTokenResponseBytes = 64 << 10
	tokenRequestTimeout   = 30 * time.Second
)

var errTokenResponseTooLarge = errors.New("the token response exceeds the size bound")

func newTokenClient(base *http.Client) *http.Client {
	transport := http.DefaultTransport
	if base.Transport != nil {
		transport = base.Transport
	}
	return &http.Client{
		Transport: &boundedTransport{next: transport, limit: maxTokenResponseBytes},
		Timeout:   tokenRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

type boundedTransport struct {
	next  http.RoundTripper
	limit int64
}

func (t *boundedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.ContentLength > t.limit {
		_ = resp.Body.Close()
		return nil, errTokenResponseTooLarge
	}
	resp.Body = &boundedBody{source: resp.Body, remaining: t.limit}
	return resp, nil
}

func (t *boundedTransport) CloseIdleConnections() {
	if closer, ok := t.next.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

type boundedBody struct {
	source    io.ReadCloser
	remaining int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		var probe [1]byte
		n, err := b.source.Read(probe[:])
		if n > 0 {
			return 0, errTokenResponseTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.source.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func (b *boundedBody) Close() error { return b.source.Close() }
