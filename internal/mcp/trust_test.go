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

	t.Run("sensitive to request timeout change", func(t *testing.T) {
		modified := *serverCfg
		modified.RequestTimeout = serverCfg.RequestTimeout + 1
		d, err := ComputeTrustDigest(&modified, tools)
		if err != nil {
			t.Fatal(err)
		}
		if d == digest1 {
			t.Fatal("expected different digest when request timeout changes")
		}
	})

	t.Run("sensitive to max message size change", func(t *testing.T) {
		modified := *serverCfg
		modified.MaxMessageSize = serverCfg.MaxMessageSize + 1
		if modified.MaxMessageSize == 0 {
			modified.MaxMessageSize = 2
		}
		d, err := ComputeTrustDigest(&modified, tools)
		if err != nil {
			t.Fatal(err)
		}
		if d == digest1 {
			t.Fatal("expected different digest when max message size changes")
		}
	})

	t.Run("nil versus empty capabilities stable", func(t *testing.T) {
		emptyCaps := *serverCfg
		emptyCaps.Capabilities = []string{}
		nilCaps := *serverCfg
		nilCaps.Capabilities = nil
		dEmpty, err := ComputeTrustDigest(&emptyCaps, tools)
		if err != nil {
			t.Fatal(err)
		}
		dNil, err := ComputeTrustDigest(&nilCaps, tools)
		if err != nil {
			t.Fatal(err)
		}
		if dEmpty != dNil {
			t.Fatalf("expected nil and empty capabilities to match: %s vs %s", dEmpty, dNil)
		}
	})

	t.Run("digest change forces new review", func(t *testing.T) {
		ctx := t.Context()
		registry := NewMemoryTrustRegistry()
		if err := registry.Approve(ctx, serverCfg.Name, digest1); err != nil {
			t.Fatal(err)
		}
		ok, err := registry.IsTrusted(ctx, serverCfg.Name, digest1)
		if err != nil || !ok {
			t.Fatalf("expected approved digest to be trusted: %v", ok)
		}
		modified := *serverCfg
		modified.RequestTimeout = serverCfg.RequestTimeout + 1
		d2, err := ComputeTrustDigest(&modified, tools)
		if err != nil {
			t.Fatal(err)
		}
		ok, err = registry.IsTrusted(ctx, serverCfg.Name, d2)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatal("expected mutated config digest to require new review")
		}
	})
}

func TestMemoryTrustRegistry(t *testing.T) {
	ctx := t.Context()
	registry := NewMemoryTrustRegistry()

	serverName := "test-server"
	digest := "aabbcc112233"

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

	err = registry.SaveSessionTrust(ctx, serverName, digest, []string{"read"}, []string{"tool_a"})
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

	trusted, err = registry.IsTrusted(ctx, serverName, "different-digest")
	if err != nil {
		t.Fatal(err)
	}
	if trusted {
		t.Fatal("expected different digest to be untrusted")
	}
}

func TestMemoryTrustRegistrySpawnApproval(t *testing.T) {
	ctx := t.Context()
	registry := NewMemoryTrustRegistry()
	serverName := "spawn-gate-server"
	digest := "spawn-digest-001"

	trusted, err := registry.IsSpawnTrusted(ctx, serverName, digest)
	if err != nil {
		t.Fatal(err)
	}
	if trusted {
		t.Fatal("expected unapproved spawn digest to be untrusted")
	}

	if err := registry.SaveSpawnTrust(ctx, serverName, digest); err != nil {
		t.Fatalf("SaveSpawnTrust failed: %v", err)
	}
	rec, err := registry.GetTrust(ctx, serverName)
	if err != nil {
		t.Fatal(err)
	}
	if rec.SpawnDecision != TrustDecisionPending || rec.SpawnDigest != digest {
		t.Fatalf("expected pending spawn record, got decision=%q digest=%q", rec.SpawnDecision, rec.SpawnDigest)
	}

	trusted, err = registry.IsSpawnTrusted(ctx, serverName, digest)
	if err != nil {
		t.Fatal(err)
	}
	if trusted {
		t.Fatal("pending spawn decision must not be trusted")
	}

	if err := registry.ApproveSpawn(ctx, serverName, digest); err != nil {
		t.Fatalf("ApproveSpawn failed: %v", err)
	}
	trusted, err = registry.IsSpawnTrusted(ctx, serverName, digest)
	if err != nil {
		t.Fatal(err)
	}
	if !trusted {
		t.Fatal("expected trusted after spawn approval")
	}

	if err := registry.SaveSpawnTrust(ctx, serverName, digest); err != nil {
		t.Fatalf("SaveSpawnTrust (same digest) failed: %v", err)
	}
	trusted, err = registry.IsSpawnTrusted(ctx, serverName, digest)
	if err != nil {
		t.Fatal(err)
	}
	if !trusted {
		t.Fatal("expected approval to survive same-digest save")
	}

	changed := "spawn-digest-002"
	if err := registry.SaveSpawnTrust(ctx, serverName, changed); err != nil {
		t.Fatalf("SaveSpawnTrust (changed digest) failed: %v", err)
	}
	trusted, err = registry.IsSpawnTrusted(ctx, serverName, changed)
	if err != nil {
		t.Fatal(err)
	}
	if trusted {
		t.Fatal("expected changed spawn digest to be untrusted until re-approval")
	}
	trusted, err = registry.IsSpawnTrusted(ctx, serverName, digest)
	if err != nil {
		t.Fatal(err)
	}
	if trusted {
		t.Fatal("expected stale spawn digest to be untrusted after change")
	}

	t.Run("invalid inputs fail", func(t *testing.T) {
		if err := registry.SaveSpawnTrust(ctx, "", digest); err == nil {
			t.Fatal("expected empty server name to fail")
		}
		if err := registry.SaveSpawnTrust(ctx, serverName, ""); err == nil {
			t.Fatal("expected empty digest to fail")
		}
		if err := registry.ApproveSpawn(ctx, "", digest); err == nil {
			t.Fatal("expected empty server name to fail on approve")
		}
		if err := registry.ApproveSpawn(ctx, serverName, ""); err == nil {
			t.Fatal("expected empty digest to fail on approve")
		}
	})
}

func TestComputeSpawnDigest(t *testing.T) {
	serverCfg := &config.MCPServer{
		Name:        "spawn-server",
		Transport:   "stdio",
		Command:     "/usr/bin/tool",
		Args:        []string{"--flag", "val"},
		Environment: map[string]string{"ENV_A": "1"},
	}

	digest, err := ComputeSpawnDigest(serverCfg)
	if err != nil {
		t.Fatalf("ComputeSpawnDigest failed: %v", err)
	}
	if digest == "" {
		t.Fatal("expected non-empty digest")
	}

	t.Run("sensitive to command change", func(t *testing.T) {
		modified := *serverCfg
		modified.Command = "/usr/bin/other"
		d, err := ComputeSpawnDigest(&modified)
		if err != nil {
			t.Fatal(err)
		}
		if d == digest {
			t.Fatal("expected different spawn digest when command changes")
		}
	})

	t.Run("sensitive to args change", func(t *testing.T) {
		modified := *serverCfg
		modified.Args = []string{"--flag", "changed"}
		d, err := ComputeSpawnDigest(&modified)
		if err != nil {
			t.Fatal(err)
		}
		if d == digest {
			t.Fatal("expected different spawn digest when args change")
		}
	})

	t.Run("sensitive to environment change", func(t *testing.T) {
		modified := *serverCfg
		modified.Environment = map[string]string{"ENV_A": "2"}
		d, err := ComputeSpawnDigest(&modified)
		if err != nil {
			t.Fatal(err)
		}
		if d == digest {
			t.Fatal("expected different spawn digest when environment changes")
		}
	})

	t.Run("insensitive to timeouts and bounds", func(t *testing.T) {
		modified := *serverCfg
		modified.RequestTimeout = serverCfg.RequestTimeout + 1
		modified.StartupTimeout = serverCfg.StartupTimeout + 1
		modified.MaxMessageSize = serverCfg.MaxMessageSize + 1
		d, err := ComputeSpawnDigest(&modified)
		if err != nil {
			t.Fatal(err)
		}
		if d != digest {
			t.Fatal("expected identical spawn digest for non-executable changes")
		}
	})

	t.Run("differs from session digest", func(t *testing.T) {
		tools := []DiscoveredTool{
			{Name: "tool_a", Description: "first", InputSchema: json.RawMessage(`{"type":"object"}`)},
		}
		sessionDigest, err := ComputeTrustDigest(serverCfg, tools)
		if err != nil {
			t.Fatal(err)
		}
		if sessionDigest == digest {
			t.Fatal("expected spawn digest to differ from session digest")
		}
	})

	t.Run("nil config fails", func(t *testing.T) {
		_, err := ComputeSpawnDigest(nil)
		if err == nil {
			t.Fatal("expected error for nil config")
		}
		if code, ok := CodeOf(err); !ok || code != ErrConfigInvalid {
			t.Fatalf("expected %s, got %s", ErrConfigInvalid, code)
		}
	})
}
