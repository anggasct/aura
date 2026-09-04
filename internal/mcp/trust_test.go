package mcp

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/anggasct/aura/internal/config"
)

func TestComputeTrustDigest(t *testing.T) {
	serverCfg := &config.MCPServer{
		Name:         "test-server",
		Transport:    "stdio",
		Command:      "/usr/bin/tool",
		Args:         []string{"--flag", "val"},
		Environment:  map[string]string{"ENV_A": "1", "ENV_B": "2"},
		Capabilities: []string{"read", "write"},
	}

	tools := []DiscoveredTool{
		{
			Name:        "tool_a",
			Description: "first tool",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"p":{"type":"string"}}}`),
		},
		{
			Name:        "tool_b",
			Description: "second tool",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
	}

	digest1, err := ComputeTrustDigest(serverCfg, tools)
	if err != nil {
		t.Fatalf("ComputeTrustDigest failed: %v", err)
	}
	if digest1 == "" {
		t.Fatal("expected non-empty digest")
	}

	// Permuting tool input order produces the same digest
	toolsReversed := []DiscoveredTool{tools[1], tools[0]}
	digest2, err := ComputeTrustDigest(serverCfg, toolsReversed)
	if err != nil {
		t.Fatalf("ComputeTrustDigest reversed failed: %v", err)
	}
	if digest1 != digest2 {
		t.Fatalf("expected deterministic digest regardless of tool order: %s vs %s", digest1, digest2)
	}

	t.Run("sensitive to command change", func(t *testing.T) {
		modified := *serverCfg
		modified.Command = "/usr/bin/different"
		d, err := ComputeTrustDigest(&modified, tools)
		if err != nil {
			t.Fatal(err)
		}
		if d == digest1 {
			t.Fatal("expected different digest when command changes")
		}
	})

	t.Run("sensitive to capabilities change", func(t *testing.T) {
		modified := *serverCfg
		modified.Capabilities = []string{"read", "write", "exec"}
		d, err := ComputeTrustDigest(&modified, tools)
		if err != nil {
			t.Fatal(err)
		}
		if d == digest1 {
			t.Fatal("expected different digest when capabilities expand")
		}
	})

	t.Run("sensitive to tool schema change", func(t *testing.T) {
		modifiedTools := []DiscoveredTool{
			{
				Name:        "tool_a",
				Description: "first tool",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"p":{"type":"number"}}}`),
			},
			tools[1],
		}
		d, err := ComputeTrustDigest(serverCfg, modifiedTools)
		if err != nil {
			t.Fatal(err)
		}
		if d == digest1 {
			t.Fatal("expected different digest when tool schema changes")
		}
	})

	t.Run("nil config fails", func(t *testing.T) {
		_, err := ComputeTrustDigest(nil, tools)
		if err == nil {
			t.Fatal("expected error for nil config")
		}
		if code, ok := CodeOf(err); !ok || code != ErrConfigInvalid {
			t.Fatalf("expected %s, got %s", ErrConfigInvalid, code)
		}
	})
}

func TestMemoryTrustRegistry(t *testing.T) {
	ctx := t.Context()
	registry := NewMemoryTrustRegistry()

	serverName := "test-server"
	digest := "aabbcc112233"

	// Initially untrusted
	trusted, err := registry.IsTrusted(ctx, serverName, digest)
	if err != nil {
		t.Fatal(err)
	}
	if trusted {
		t.Fatal("expected initially untrusted")
	}

	rec, err := registry.GetTrust(ctx, serverName)
	if !errors.Is(err, ErrTrustNotFound) {
		t.Fatalf("expected ErrTrustNotFound, got %v", err)
	}
	if rec != nil {
		t.Fatal("expected nil record initially")
	}

	// Save pending
	err = registry.SaveTrust(ctx, &TrustRecord{
		ServerName:   serverName,
		Digest:       digest,
		Decision:     TrustDecisionPending,
		Capabilities: []string{"read"},
		Tools:        []string{"tool_a"},
	})
	if err != nil {
		t.Fatal(err)
	}

	trusted, err = registry.IsTrusted(ctx, serverName, digest)
	if err != nil {
		t.Fatal(err)
	}
	if trusted {
		t.Fatal("pending decision must not be trusted")
	}

	// Approve
	err = registry.Approve(ctx, serverName, digest)
	if err != nil {
		t.Fatal(err)
	}

	trusted, err = registry.IsTrusted(ctx, serverName, digest)
	if err != nil {
		t.Fatal(err)
	}
	if !trusted {
		t.Fatal("expected trusted after approval")
	}

	// Different digest is not trusted
	trusted, err = registry.IsTrusted(ctx, serverName, "different-digest")
	if err != nil {
		t.Fatal(err)
	}
	if trusted {
		t.Fatal("expected different digest to be untrusted")
	}
}
