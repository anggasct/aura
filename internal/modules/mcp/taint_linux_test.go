//go:build linux

package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/workspace"
)

func TestTaintedMCPCallRecordsWorkspaceChanges(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	db := sqlitetest.OpenMigrated(t, workspace.Schema())
	store := workspace.NewTaintStore(db)
	tracker := workspace.NewTracker(root, store, workspace.DefaultScanEntries, nil, slog.New(slog.DiscardHandler))

	server := sdk.NewServer(&sdk.Implementation{Name: "fs-server", Version: "1.0.0"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "drop_file", Description: "writes a file into workspace"}, func(_ context.Context, _ *sdk.CallToolRequest, _ map[string]any) (*sdk.CallToolResult, map[string]any, error) {
		if err := os.WriteFile(filepath.Join(root, "mcp-out.txt"), []byte("written by mcp"), 0o600); err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"written": "mcp-out.txt"}, nil
	})
	clientTransport, serverTransport := sdk.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, serverTransport) }()

	client, err := NewClient(&ServerConfig{
		Name:           "fs-server",
		Transport:      TransportStdio,
		RequestTimeout: configkit.Duration(5 * time.Second),
		StartupTimeout: configkit.Duration(5 * time.Second),
		MaxMessageSize: DefaultMaxMessageSize,
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close(t.Context()) }()
	if err := client.Connect(ctx, clientTransport); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	adapter := NewAdapterWithTracker(client, "drop_file", 4096, tracker)
	req := &toolbroker.ToolRequest{
		ToolName:    "mcp_fs-server_drop_file",
		ToolVersion: "v1",
		SessionID:   "sess-mcp",
		Arguments:   json.RawMessage(`{}`),
		Tainted:     true,
		TaintMark:   turn.TaintMark{Source: turn.TaintSourceToolResult, Detail: "web_fetch"},
	}
	res, err := adapter(ctx, req, approval.Constraints{MaxOutputBytes: 4096, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	if !res.Untrusted {
		t.Errorf("mcp result Untrusted = false, want true")
	}
	rows, err := store.List(ctx, "")
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	if len(rows) != 1 || rows[0].Path != "mcp-out.txt" {
		t.Fatalf("workspace_taint rows = %+v, want mcp-out.txt recorded", rows)
	}
}
