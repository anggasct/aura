package auth

import (
	"context"
	"net/http"
	"strings"
)

type none struct{}

func newNone(_ *Registry, _ *Spec) (Strategy, error) {
	return none{}, nil
}

func (none) Ready(context.Context) error { return nil }

func (none) Authorize(context.Context, *http.Request) error { return nil }

type staticSecret struct {
	registry *Registry
	header   string
	ref      string
}

func newStaticSecret(r *Registry, spec *Spec) (Strategy, error) {
	if spec.SecretRef == "" {
		return nil, Errorf(ErrInvalid, "static_secret needs a secret reference")
	}
	header := spec.Header
	if header == "" {
		header = DefaultHeader
	}
	return &staticSecret{registry: r, header: header, ref: spec.SecretRef}, nil
}

func (s *staticSecret) resolve(ctx context.Context) (string, error) {
	value, err := s.registry.secrets.ResolveSecret(ctx, s.ref)
	value = strings.TrimSpace(value)
	if err != nil || value == "" {
		return "", Errorf(ErrRequired, "the plugin secret is unavailable")
	}
	s.registry.register(value)
	return value, nil
}

func (s *staticSecret) Ready(ctx context.Context) error {
	_, err := s.resolve(ctx)
	return err
}

func (s *staticSecret) Authorize(ctx context.Context, req *http.Request) error {
	if req == nil {
		return Errorf(ErrInvalid, "a request is required")
	}
	value, err := s.resolve(ctx)
	if err != nil {
		return err
	}
	if strings.EqualFold(s.header, DefaultHeader) && !strings.Contains(value, " ") {
		value = "Bearer " + value
	}
	req.Header.Set(s.header, value)
	return nil
}
