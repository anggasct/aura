package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/durable/restate"
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
	for _, want := range []string{"mode: external", "ingress: reachable", "admin: reachable", "deployment: absent"} {
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
	listener, err := buildDurableListener(t.Context(), &cfg, db, nil)
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
