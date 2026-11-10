package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/durable/restate"
)

func newDurableStatusCmdForTest(t *testing.T, cfg *config.Config) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	cmd := &cobra.Command{Use: "status"}
	cmd.SetContext(t.Context())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.RunE = func(c *cobra.Command, _ []string) error {
		return runDurableStatus(c, cfg)
	}
	return cmd, &out
}

func TestDurableStatusDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.Durable.Enabled = false
	cmd, out := newDurableStatusCmdForTest(t, &cfg)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if !strings.Contains(out.String(), "durable: disabled") {
		t.Errorf("output = %q, want disabled line", out.String())
	}
}

func TestDurableStatusUnreachableFailsClosed(t *testing.T) {
	cfg := config.Default()
	cfg.Durable.Enabled = true
	cfg.Durable.Mode = config.DurableModeExternal
	cfg.Durable.Endpoint = "http://127.0.0.1:1"
	cfg.Durable.AdminEndpoint = "http://127.0.0.1:1"
	cmd, out := newDurableStatusCmdForTest(t, &cfg)
	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("expected unreachable durable runtime to fail, got nil")
	}
	if code, ok := restate.CodeOf(err); !ok || code != restate.ErrorCodeUnreachable {
		t.Fatalf("err = %v, want %s", err, restate.ErrorCodeUnreachable)
	}
	for _, want := range []string{"mode: external", "ingress: unreachable", "admin: unreachable"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output = %q, want %q", out.String(), want)
		}
	}
}

func TestDurableStatusReachable(t *testing.T) {
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/restate/health", "/version":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case "/deployments":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"deployments":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(probe.Close)
	cfg := config.Default()
	cfg.Durable.Enabled = true
	cfg.Durable.Mode = config.DurableModeExternal
	cfg.Durable.Endpoint = probe.URL
	cfg.Durable.AdminEndpoint = probe.URL
	cmd, out := newDurableStatusCmdForTest(t, &cfg)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	for _, want := range []string{"mode: external", "ingress: reachable", "admin: reachable", "deployment: absent", "node_addr: 127.0.0.1:5122"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output = %q, want %q", out.String(), want)
		}
	}
}

func TestBuildDurableListenerExternalUnreachableFailsClosed(t *testing.T) {
	dataRoot := t.TempDir()
	seedHealthyStorage(t, dataRoot)
	cfg := config.Default()
	cfg.Storage.Path = dataRoot
	cfg.Tools = nil
	cfg.Models.Definitions = map[string]config.ModelDefinition{
		"primary": {Protocol: "anthropic", Model: "claude-sonnet-4"},
	}
	cfg.Durable.Enabled = true
	cfg.Durable.Mode = config.DurableModeExternal
	cfg.Durable.Endpoint = "http://127.0.0.1:1"
	cfg.Durable.AdminEndpoint = "http://127.0.0.1:1"
	db, err := openStorage(t.Context(), &cfg)
	if err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	defer func() { _ = db.Close() }()
	listener, err := buildDurableListener(t.Context(), loadResultWithAll(t, &cfg), db, nil, nil)
	if err != nil {
		t.Fatalf("buildDurableListener: %v", err)
	}
	if listener == nil {
		t.Fatal("expected a listener in external mode, got nil")
	}
	if err := listener.Start(t.Context()); err == nil {
		t.Fatal("expected unreachable external runtime to fail Start, got nil")
	} else if code, ok := restate.CodeOf(err); !ok || code != restate.ErrorCodeUnreachable {
		t.Fatalf("Start err = %v, want %s", err, restate.ErrorCodeUnreachable)
	}
}

func TestExternalListenerRegistersItsHandlerURL(t *testing.T) {
	registered := make(chan string, 1)
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/restate/health" || r.URL.Path == "/version":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/deployments" && r.Method == http.MethodPost:
			var body struct {
				URI string `json:"uri"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			select {
			case registered <- body.URI:
			default:
			}
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(admin.Close)
	dataRoot := t.TempDir()
	seedHealthyStorage(t, dataRoot)
	cfg := config.Default()
	cfg.Storage.Path = dataRoot
	cfg.Tools = nil
	cfg.Models.Definitions = map[string]config.ModelDefinition{
		"primary": {Protocol: "anthropic", Model: "claude-sonnet-4"},
	}
	cfg.Durable.Enabled = true
	cfg.Durable.Mode = config.DurableModeExternal
	cfg.Durable.Endpoint = admin.URL
	cfg.Durable.AdminEndpoint = admin.URL
	cfg.Durable.HandlerAddr = "127.0.0.1:0"
	cfg.Durable.HandlerURL = "http://aura:9080"
	db, err := openStorage(t.Context(), &cfg)
	if err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	defer func() { _ = db.Close() }()
	listener, err := buildDurableListener(t.Context(), loadResultWithAll(t, &cfg), db, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatalf("buildDurableListener: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- listener.Start(ctx) }()
	select {
	case uri := <-registered:
		if uri != "http://aura:9080" {
			t.Fatalf("registered uri = %q", uri)
		}
	case err := <-done:
		t.Fatalf("Start returned before registering: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the handler deployment was never registered")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start after shutdown = %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("listener never stopped")
	}
}

func TestEndpointCancelledAroundItsBindStopsCleanly(t *testing.T) {
	dataRoot := t.TempDir()
	seedHealthyStorage(t, dataRoot)
	cfg := config.Default()
	cfg.Storage.Path = dataRoot
	cfg.Tools = nil
	cfg.Models.Definitions = map[string]config.ModelDefinition{
		"primary": {Protocol: "anthropic", Model: "claude-sonnet-4"},
	}
	cfg.Durable.Enabled = true
	cfg.Durable.Mode = config.DurableModeExternal
	cfg.Durable.HandlerAddr = "127.0.0.1:0"
	cfg.Durable.HandlerURL = "http://aura:9080"
	db, err := openStorage(t.Context(), &cfg)
	if err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	defer func() { _ = db.Close() }()
	for name, delay := range map[string]time.Duration{"before start": -1, "right after start": 0, "after bind": 50 * time.Millisecond} {
		t.Run(name, func(t *testing.T) {
			listener, err := buildDurableListener(t.Context(), loadResultWithAll(t, &cfg), db, slog.New(slog.DiscardHandler), nil)
			if err != nil {
				t.Fatalf("buildDurableListener: %v", err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			if delay < 0 {
				cancel()
			} else {
				time.AfterFunc(delay, cancel)
			}
			defer cancel()
			if err := listener.serveEndpointUntilDone(ctx); err != nil {
				t.Fatalf("serveEndpointUntilDone after cancel = %v, want nil", err)
			}
		})
	}
}

func TestDurableStatusMatchesTheRegisteredHandlerURL(t *testing.T) {
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/restate/health", "/version":
			w.WriteHeader(http.StatusOK)
		case "/deployments":
			_, _ = w.Write([]byte(`{"deployments":[{"uri":"http://aura:9080/"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(probe.Close)
	cfg := config.Default()
	cfg.Durable.Enabled = true
	cfg.Durable.Mode = config.DurableModeExternal
	cfg.Durable.Endpoint = probe.URL
	cfg.Durable.AdminEndpoint = probe.URL
	cfg.Durable.HandlerAddr = "0.0.0.0:9080"
	cfg.Durable.HandlerURL = "http://aura:9080"
	cmd, out := newDurableStatusCmdForTest(t, &cfg)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	for _, want := range []string{"handler_url: http://aura:9080", "deployment: registered"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output = %q, want %q", out.String(), want)
		}
	}
}
