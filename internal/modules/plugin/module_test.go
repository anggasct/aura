package plugin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/plugin/connector"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/store/storetest"
)

func TestModuleNewRejectsInvalidDeps(t *testing.T) {
	db := storetest.OpenMigrated(t)
	var nilCtx context.Context
	if _, err := New(nilCtx, &ModuleDeps{DB: db, DataRoot: t.TempDir()}); !fault.HasCode(err, ErrInvalidArgument) {
		t.Fatalf("New(nil ctx) = %v, want %s", err, ErrInvalidArgument)
	}
	cases := []struct {
		name string
		deps *ModuleDeps
	}{
		{"nil deps", nil},
		{"nil db", &ModuleDeps{DataRoot: t.TempDir()}},
		{"empty data root", &ModuleDeps{DB: db}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(t.Context(), tc.deps); !fault.HasCode(err, ErrInvalidArgument) {
				t.Fatalf("New = %v, want %s", err, ErrInvalidArgument)
			}
		})
	}
}

func TestModuleEmptyHealthCheckAndClose(t *testing.T) {
	db := storetest.OpenMigrated(t)
	mod, err := New(t.Context(), &ModuleDeps{
		DB:       db,
		DataRoot: t.TempDir(),
		Logger:   slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok, err := mod.HealthCheck(nil, time.Second); err != nil || ok {
		t.Fatalf("HealthCheck on empty module = %v, %v; want false, nil", ok, err)
	}
	if len(mod.MCPBackends()) != 0 || len(mod.ConnectorStatuses()) != 0 || mod.HasConnectors() {
		t.Fatalf("empty module reported active backends")
	}
	if err := mod.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

type probeConnectorStarter struct {
	targetURL string
}

func (s *probeConnectorStarter) StartSession(context.Context, *connector.SessionRequest) (connector.Session, error) {
	return &probeConnectorSession{targetURL: s.targetURL}, nil
}

type probeConnectorSession struct {
	targetURL string
}

func (s *probeConnectorSession) Request(_ context.Context, payload []byte) ([]byte, error) {
	var msg struct {
		Type   string `json:"type"`
		Call   string `json:"call"`
		Error  string `json:"error"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal(payload, &msg); err != nil {
		return nil, err
	}
	switch msg.Type {
	case "describe":
		return json.Marshal(map[string]any{
			"v":     connector.Version,
			"type":  "tools",
			"tools": []map[string]any{{"name": "probe", "description": "Probe endpoint.", "input_schema": json.RawMessage(`{"type":"object"}`)}},
		})
	case "invoke":
		return json.Marshal(map[string]any{
			"v":      connector.Version,
			"type":   "http",
			"call":   msg.Call,
			"method": http.MethodGet,
			"url":    s.targetURL,
		})
	case "http_result":
		return json.Marshal(map[string]any{
			"v":      connector.Version,
			"type":   "result",
			"call":   msg.Call,
			"output": map[string]any{"status": msg.Status, "error": msg.Error},
		})
	default:
		return json.Marshal(map[string]any{"v": connector.Version, "type": "error", "code": "bad_request"})
	}
}

func (*probeConnectorSession) Close(context.Context) error { return nil }

func TestModuleKeepsAuthAndConnectorHTTPClientsSeparate(t *testing.T) {
	var loopbackHits atomic.Int64
	loopbackServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		loopbackHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(loopbackServer.Close)
	loopbackHost := strings.TrimPrefix(loopbackServer.URL, "https://")

	db := storetest.OpenMigrated(t)
	dataRoot := t.TempDir()
	service, err := OpenService(db, dataRoot, nil, loopbackServer.Client())
	if err != nil {
		t.Fatalf("OpenService: %v", err)
	}
	manifest := `api_version: aura.plugin/v1
id: probe
version: 1.0.0
name: Probe
description: Loopback SSRF probe.
capabilities: []
tools:
  - name: probe
    description: Probe endpoint.
    tier: read
egress: [` + loopbackHost + `]
secrets: []
auth:
  kind: none
backend:
  kind: connector
  command: ./bin/connector
`
	bundleDir := writeBundle(t, manifest, map[string]string{"bin/connector": "#!/bin/sh\n"})
	if err := os.Chmod(filepath.Join(bundleDir, "bin", "connector"), 0o700); err != nil {
		t.Fatalf("chmod connector: %v", err)
	}
	installed, err := service.Install(t.Context(), bundleDir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := service.Approve(t.Context(), installed.ID, installed.Digest[:16]); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	broker, err := toolbroker.New(&toolbroker.Options{})
	if err != nil {
		t.Fatalf("toolbroker.New: %v", err)
	}
	passthroughValidator := func(json.RawMessage, int64) func(json.RawMessage) (json.RawMessage, error) {
		return func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil }
	}
	mod, err := New(t.Context(), &ModuleDeps{
		DB:             db,
		DataRoot:       dataRoot,
		Broker:         broker,
		Validator:      passthroughValidator,
		AuthHTTPClient: loopbackServer.Client(),
		Starter:        &probeConnectorStarter{targetURL: loopbackServer.URL + "/secret"},
		Logger:         slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = mod.Close(context.WithoutCancel(t.Context())) })
	if statuses := mod.ConnectorStatuses(); len(statuses) != 1 || statuses[0].Code != "" {
		t.Fatalf("ConnectorStatuses = %+v, want 1 running connector", statuses)
	}

	res, err := broker.Execute(t.Context(), &toolbroker.ToolRequest{
		RequestID:      "req-1",
		TurnID:         "turn-1",
		SessionID:      "session-1",
		PrincipalID:    "owner-1",
		PrincipalClass: principal.ClassOwner,
		ToolName:       "plugin_probe_probe",
		ToolVersion:    "v1",
		Arguments:      json.RawMessage(`{}`),
		Trust:          approval.TrustOwnerInput,
		IdempotencyKey: "idem-1",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if loopbackHits.Load() != 0 {
		t.Fatalf("connector hit loopback server %d times, want 0 (strict egress must not share AuthHTTPClient)", loopbackHits.Load())
	}
	if !strings.Contains(string(res.Output), "egress_denied") {
		t.Fatalf("output = %s, want egress_denied from strict ConnectorHTTPClient", res.Output)
	}
}
