package auth

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/plugin/auth/authtest"
)

type mapSecrets struct {
	mu     sync.Mutex
	values map[string]string
}

func (m *mapSecrets) ResolveSecret(_ context.Context, ref string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.values[ref]
	if !ok {
		return "", Errorf(ErrInvalid, "unknown reference")
	}
	return value, nil
}

func (m *mapSecrets) set(ref, value string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[ref] = value
}

type loggedRedactor struct {
	mu     sync.Mutex
	values []string
}

func (l *loggedRedactor) RegisterSecret(value string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.values = append(l.values, value)
}

func (l *loggedRedactor) has(value string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Contains(l.values, value)
}

const (
	testPluginID    = "mailer"
	testFingerprint = "fingerprint-one"
	clientIDRef     = "env://MAILER_CLIENT_ID"
	clientSecretRef = "env://MAILER_CLIENT_SECRET"
	staticRef       = "env://MAILER_TOKEN"
)

type harness struct {
	t        *testing.T
	as       *authtest.Server
	dir      string
	secrets  *mapSecrets
	redactor *loggedRedactor
	listener net.Listener
	redirect string
	now      time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return &harness{
		t:        t,
		as:       authtest.New(t),
		dir:      filepath.Join(t.TempDir(), "plugin-secrets"),
		secrets:  &mapSecrets{values: map[string]string{clientIDRef: authtest.ClientID, clientSecretRef: authtest.Secret, staticRef: "static-token-value"}},
		redactor: &loggedRedactor{},
		listener: listener,
		redirect: "http://" + listener.Addr().String() + "/callback",
		now:      time.Now(),
	}
}

func (h *harness) registry() *Registry {
	h.t.Helper()
	registry, err := NewRegistry(&Options{
		Dir: h.dir, Secrets: h.secrets, HTTPClient: http.DefaultClient, Redactor: h.redactor, Now: func() time.Time { return h.now },
		Listen: func(_ context.Context, address string) (net.Listener, error) {
			if address != h.listener.Addr().String() {
				return nil, Errorf(ErrInvalid, "unexpected address")
			}
			return h.listener, nil
		},
	})
	if err != nil {
		h.t.Fatalf("NewRegistry: %v", err)
	}
	return registry
}

func (h *harness) spec(mutate func(*Spec)) *Spec {
	spec := &Spec{
		PluginID: testPluginID, Kind: KindOAuth2, Fingerprint: testFingerprint,
		AuthorizeURL: h.as.AuthorizeURL(), TokenURL: h.as.TokenURL(), RedirectURI: h.redirect,
		Scopes: []string{"mail.read"}, ClientIDRef: clientIDRef, ClientSecretRef: clientSecretRef,
	}
	if mutate != nil {
		mutate(spec)
	}
	return spec
}

func (h *harness) strategy(spec *Spec) Interactive {
	h.t.Helper()
	strategy, err := h.registry().Strategy(spec)
	if err != nil {
		h.t.Fatalf("Strategy: %v", err)
	}
	interactive, ok := strategy.(Interactive)
	if !ok {
		h.t.Fatalf("strategy %T is not interactive", strategy)
	}
	return interactive
}

func (h *harness) tokenPath() string {
	return filepath.Join(h.dir, testPluginID, tokenFileName)
}

func (h *harness) loggedIn(spec *Spec) Interactive {
	h.t.Helper()
	strategy := h.strategy(spec)
	if err := h.pasteLogin(strategy, nil); err != nil {
		h.t.Fatalf("Login: %v", err)
	}
	return strategy
}

type pastedInput struct {
	mu   sync.Mutex
	url  string
	read bool
}

func (p *pastedInput) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.read {
		return 0, os.ErrClosed
	}
	p.read = true
	return copy(b, p.url), nil
}

func (h *harness) pasteLogin(strategy Interactive, edit func(callback string) string) error {
	h.t.Helper()
	input := &pastedInput{}
	opts := &LoginOptions{
		Paste: true,
		Input: input,
		Prompt: func(authorizeURL string) {
			callback := h.as.Approve(authorizeURL)
			if edit != nil {
				callback = edit(callback)
			}
			input.mu.Lock()
			input.url = callback + "\n"
			input.mu.Unlock()
		},
	}
	return strategy.Login(h.t.Context(), opts)
}
