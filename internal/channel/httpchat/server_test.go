package httpchat

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/principal"
)

func newTestServer(t *testing.T, fixture *handlerFixture, listen string, logs io.Writer) *Server {
	t.Helper()
	server, err := NewServer(fixture.handler, listen, time.Second, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return server
}

func TestServerServesAndDrainsOnShutdown(t *testing.T) {
	fixture := newFixture(t, fixtureOptions{})
	issued := fixture.keys.add(t, principal.ClassTrusted, "kantor")
	server := newTestServer(t, fixture, "127.0.0.1:0", io.Discard)
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+listener.Addr().String()+routeModels, http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+issued.Plaintext)
	client := &http.Client{Timeout: testWait}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET models: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"id":"kantor"`) {
		t.Fatalf("GET models = %d %q, %v", resp.StatusCode, body, err)
	}
	if findings := server.Check(ctx); len(findings) != 0 {
		t.Fatalf("serving listener reports %+v", findings)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("Serve did not return after shutdown")
	}
}

type signalHandler struct {
	slog.Handler
	message string
	seen    chan struct{}
}

func (h *signalHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *signalHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == h.message {
		close(h.seen)
	}
	return h.Handler.Handle(ctx, record)
}

func TestServerBindFailureReportsListenerDown(t *testing.T) {
	occupied, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = occupied.Close() }()
	fixture := newFixture(t, fixtureOptions{})
	seen := make(chan struct{})
	logger := slog.New(&signalHandler{Handler: slog.DiscardHandler, message: "http listener is not bindable", seen: seen})
	server, err := NewServer(fixture.handler, occupied.Addr().String(), time.Second, logger)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if findings := server.Check(t.Context()); len(findings) != 0 {
		t.Fatalf("listener that has not started reports %+v", findings)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Start(ctx) }()

	select {
	case <-seen:
	case <-time.After(testWait):
		t.Fatal("bind failure was not reported")
	}
	findings := server.Check(ctx)
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want one http_listener_down", findings)
	}
	got := findings[0]
	if got.Code != FindingListenerDown || got.Component != "http" || got.Status != health.StatusDown || got.Remediation != RemediationReviewListener || got.Detail == "" {
		t.Fatalf("finding = %+v", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start after a bind failure returned %v, want a quiet wait for shutdown", err)
		}
	case <-time.After(testWait):
		t.Fatal("Start did not return after shutdown")
	}
}

func TestNewServerValidatesArguments(t *testing.T) {
	fixture := newFixture(t, fixtureOptions{})
	logger := slog.New(slog.DiscardHandler)
	cases := map[string]func() (*Server, error){
		"nil handler":  func() (*Server, error) { return NewServer(nil, "127.0.0.1:1", time.Second, logger) },
		"empty listen": func() (*Server, error) { return NewServer(fixture.handler, "", time.Second, logger) },
		"zero grace":   func() (*Server, error) { return NewServer(fixture.handler, "127.0.0.1:1", 0, logger) },
		"nil logger":   func() (*Server, error) { return NewServer(fixture.handler, "127.0.0.1:1", time.Second, nil) },
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			server, err := build()
			if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument || server != nil {
				t.Fatalf("NewServer = %v, %v; want %s", server, err, ErrorCodeInvalidArgument)
			}
		})
	}
}
