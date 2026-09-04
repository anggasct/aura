package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/toolbroker"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestFormatToolName(t *testing.T) {
	got := FormatToolName("local-files", "read_file")
	want := "mcp_local-files_read_file"
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestMakeValidator(t *testing.T) {
	v := MakeValidator(nil, 64)

	t.Run("empty raw returns empty object", func(t *testing.T) {
		res, err := v(nil)
		if err != nil {
			t.Fatal(err)
		}
		if string(res) != "{}" {
			t.Fatalf("got %s, want {}", string(res))
		}
	})

	t.Run("valid JSON passes", func(t *testing.T) {
		valid := json.RawMessage(`{"key":"value"}`)
		res, err := v(valid)
		if err != nil {
			t.Fatal(err)
		}
		if string(res) != string(valid) {
			t.Fatalf("got %s, want %s", string(res), string(valid))
		}
	})

	t.Run("invalid JSON fails", func(t *testing.T) {
		invalid := json.RawMessage(`{invalid`)
		_, err := v(invalid)
		if err == nil {
			t.Fatal("expected error for invalid JSON")
		}
	})

	t.Run("oversized JSON fails", func(t *testing.T) {
		oversized := json.RawMessage(`{"long":"123456789012345678901234567890123456789012345678901234567890"}`)
		_, err := v(oversized)
		if err == nil {
			t.Fatal("expected error for oversized JSON")
		}
	})

	t.Run("schema violation fails without reaching transport", func(t *testing.T) {
		schema := json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"],"additionalProperties":false}`)
		vs := MakeValidator(schema, 1024)
		bad := json.RawMessage(`{"count":"not-an-integer"}`)
		if _, err := vs(bad); err == nil {
			t.Fatal("expected schema violation to fail")
		}
		good := json.RawMessage(`{"count":3}`)
		if _, err := vs(good); err != nil {
			t.Fatalf("expected valid args to pass: %v", err)
		}
	})
}

func TestAdapterNilGuards(t *testing.T) {
	ctx := t.Context()
	constraints := approval.Constraints{
		MaxOutputBytes: 1024,
		Timeout:        5 * time.Second,
	}

	t.Run("nil client returns capability unavailable", func(t *testing.T) {
		adapter := NewAdapter(nil, "tool", 1024)
		req := &toolbroker.ToolRequest{ToolName: "mcp_srv_tool", ToolVersion: "v1"}
		_, err := adapter(ctx, req, constraints)
		if err == nil {
			t.Fatal("expected error")
		}
		if code, _ := toolbroker.CodeOf(err); code != toolbroker.ResultCapabilityUnavailable {
			t.Fatalf("got %s, want %s", code, toolbroker.ResultCapabilityUnavailable)
		}
	})

	t.Run("nil request returns invalid argument", func(t *testing.T) {
		client := &Client{}
		adapter := NewAdapter(client, "tool", 1024)
		_, err := adapter(ctx, nil, constraints)
		if err == nil {
			t.Fatal("expected error")
		}
		if code, _ := toolbroker.CodeOf(err); code != toolbroker.ResultInvalidArgument {
			t.Fatalf("got %s, want %s", code, toolbroker.ResultInvalidArgument)
		}
	})
}

func TestAdapterOversizedResultFails(t *testing.T) {
	ctx := t.Context()
	server := sdk.NewServer(&sdk.Implementation{Name: "big-server", Version: "1.0.0"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "big", Description: "returns large output"}, func(_ context.Context, _ *sdk.CallToolRequest, _ map[string]any) (*sdk.CallToolResult, map[string]any, error) {
		return nil, map[string]any{"blob": strings.Repeat("x", 4096)}, nil
	})
	clientTransport, serverTransport := sdk.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, serverTransport) }()
	serverCfg := &config.MCPServer{
		Name:           "big-server",
		Transport:      config.MCPTransportStdio,
		RequestTimeout: config.Duration(5 * time.Second),
		StartupTimeout: config.Duration(5 * time.Second),
	}
	client, err := NewClient(serverCfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	if err := client.Connect(ctx, clientTransport); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	adapter := NewAdapter(client, "big", 64)
	req := &toolbroker.ToolRequest{ToolName: "mcp_big-server_big", ToolVersion: "v1", Arguments: json.RawMessage(`{}`)}
	constraints := approval.Constraints{MaxOutputBytes: 64, Timeout: 5 * time.Second}
	_, err = adapter(ctx, req, constraints)
	if err == nil {
		t.Fatal("expected oversized result to fail")
	}
	if code, _ := toolbroker.CodeOf(err); code != toolbroker.ResultExecutionFailed {
		t.Fatalf("expected %s, got %s (%v)", toolbroker.ResultExecutionFailed, code, err)
	}
}
