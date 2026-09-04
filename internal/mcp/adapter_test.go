package mcp

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/toolbroker"
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
