package mcp

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

func TestModuleRejectsInvalidDeps(t *testing.T) {
	if _, err := New(t.Context(), nil); !fault.HasCode(err, ErrConfigInvalid) {
		t.Fatalf("nil deps = %v, want %s", err, ErrConfigInvalid)
	}
	if _, err := New(t.Context(), &ModuleDeps{}); !fault.HasCode(err, ErrConfigInvalid) {
		t.Fatalf("empty deps = %v, want %s", err, ErrConfigInvalid)
	}
	broker, err := toolbroker.New(&toolbroker.Options{HostCapabilities: capabilitytest.Host(t)})
	if err != nil {
		t.Fatalf("toolbroker.New: %v", err)
	}
	if _, err := New(t.Context(), &ModuleDeps{Broker: broker}); !fault.HasCode(err, ErrConfigInvalid) {
		t.Fatalf("no servers or plugins = %v, want %s", err, ErrConfigInvalid)
	}
}

func TestModuleHealthChecksAndNilClose(t *testing.T) {
	var nilMod *Module
	if err := nilMod.Close(t.Context()); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
	if checks := nilMod.HealthChecks(); len(checks) != 0 {
		t.Fatalf("nil HealthChecks = %v, want empty", checks)
	}

	broker, err := toolbroker.New(&toolbroker.Options{HostCapabilities: capabilitytest.Host(t)})
	if err != nil {
		t.Fatalf("toolbroker.New: %v", err)
	}
	mod, err := New(t.Context(), &ModuleDeps{
		Config: &config.MCP{
			Servers: []config.MCPServer{
				{
					Name:           "docs",
					Transport:      config.MCPTransportStdio,
					Command:        "/usr/bin/docs-mcp",
					StartupTimeout: configkit.Duration(5 * time.Second),
					RequestTimeout: configkit.Duration(5 * time.Second),
					MaxMessageSize: 1 << 20,
				},
			},
		},
		Broker:       broker,
		TrustPath:    filepath.Join(t.TempDir(), "mcp", "trust.json"),
		CheckTimeout: 3 * time.Second,
		Logger:       slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = mod.Close(context.WithoutCancel(t.Context())) })

	checks := mod.HealthChecks()
	if len(checks) != 1 || checks[0].ID != HealthCheckID || checks[0].Remediation != RemediationReviewServer || checks[0].Timeout != 3*time.Second {
		t.Fatalf("HealthChecks = %+v", checks)
	}
	if mod.Manager() == nil {
		t.Fatal("Manager() = nil")
	}
}
