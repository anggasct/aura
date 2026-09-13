//go:build linux

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/mcp"
	"github.com/anggasct/aura/internal/sandbox"
)

func containedFixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/mcp-stdio-helper.sh")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp-stdio-helper.sh")
	if err := os.WriteFile(path, raw, 0o700); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestContainedRealSessionExchange(t *testing.T) {
	ctx := t.Context()
	helper := containedFixture(t)
	workDir := filepath.Dir(helper)

	probe, err := newMCPSessionStarter(sandbox.Start).StartSession(ctx, &mcp.ContainedSessionRequest{
		RequestID: "probe", ServerName: "probe", Executable: helper,
		WorkingDir: workDir, Environment: map[string]string{},
		Timeout: 30 * time.Second, MaxOutputBytes: 1 << 20,
	})
	if err != nil {
		if code, ok := mcp.CodeOf(err); ok && code == mcp.ErrServerUnavailable {
			t.Skipf("containment unavailable on this host: %v", err)
		}
		t.Fatalf("StartSession(): %v", err)
	}
	_ = probe.Close(ctx)

	serverCfg := &config.MCPServer{
		Name:           "contained-real",
		Transport:      config.MCPTransportStdio,
		Command:        helper,
		Environment:    map[string]string{"SERVER_MODE": "contained"},
		StartupTimeout: config.Duration(30 * time.Second),
		RequestTimeout: config.Duration(30 * time.Second),
		MaxMessageSize: 1 << 20,
	}
	client, err := mcp.NewClient(serverCfg, nil, mcp.WithSessionStarter(newMCPSessionStarter(sandbox.Start)))
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	defer func() { _ = client.Close(ctx) }()
	if err := client.Connect(ctx, nil); err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	tools, err := client.DiscoverTools(ctx)
	if err != nil {
		t.Fatalf("DiscoverTools(): %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "ping" {
		t.Fatalf("tools = %+v", tools)
	}
	args, err := json.Marshal(map[string]string{"message": "hi"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res, err := client.CallTool(ctx, "ping", args)
	if err != nil {
		t.Fatalf("CallTool(): %v", err)
	}
	if res.IsError {
		t.Fatal("tool call returned error result")
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if !strings.Contains(string(raw), "mode=contained") {
		t.Errorf("declared environment missing from result: %s", raw)
	}
	if !strings.Contains(string(raw), "home=unset") {
		t.Errorf("undeclared environment leaked into session: %s", raw)
	}
}
