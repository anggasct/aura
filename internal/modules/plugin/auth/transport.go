package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/secret"
)

type loopbackAwareTransport struct {
	loopback http.RoundTripper
	external http.RoundTripper
}

func (t *loopbackAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req != nil && req.URL != nil && egress.IsLoopbackBaseURL(req.URL.String()) {
		return t.loopback.RoundTrip(req)
	}
	return t.external.RoundTrip(req)
}

func (t *loopbackAwareTransport) CloseIdleConnections() {
	for _, next := range []http.RoundTripper{t.loopback, t.external} {
		if closer, ok := next.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
	}
}

func NewHTTPClient(resolver egress.Resolver) *http.Client {
	external := egress.NewClient(resolver)
	return &http.Client{
		Transport: &loopbackAwareTransport{
			loopback: http.DefaultTransport,
			external: external.Transport,
		},
	}
}

type secretResolver struct{}

func NewSecretResolver() SecretResolver {
	return secretResolver{}
}

func (secretResolver) ResolveSecret(_ context.Context, raw string) (string, error) {
	ref, err := secret.ParseRef(raw)
	if err != nil || ref.IsZero() {
		return "", Errorf(ErrInvalid, "credential reference must use env:// or file://")
	}
	value, err := ref.Reference().Resolve()
	if err != nil || strings.TrimSpace(value) == "" {
		return "", Errorf(ErrRequired, "credential is unavailable")
	}
	return value, nil
}
