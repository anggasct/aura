package mcp

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

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

func trustRegistryFactories(t *testing.T) map[string]func() TrustRegistry {
	t.Helper()
	return map[string]func() TrustRegistry{
		"memory": func() TrustRegistry { return NewMemoryTrustRegistry() },
		"file": func() TrustRegistry {
			registry, err := NewFileTrustRegistry(filepath.Join(t.TempDir(), "mcp", "trust.json"))
			if err != nil {
				t.Fatalf("NewFileTrustRegistry(): %v", err)
			}
			return registry
		},
	}
}

func TestTrustRegistryConfigApproval(t *testing.T) {
	for name, factory := range trustRegistryFactories(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			registry := factory()
			if trusted, err := registry.IsConfigTrusted(ctx, "srv", "cfg-1"); err != nil || trusted {
				t.Fatalf("unapproved config trusted: %v, %v", trusted, err)
			}
			if err := registry.ApproveConfig(ctx, "srv", "cfg-1"); err != nil {
				t.Fatalf("ApproveConfig(): %v", err)
			}
			if trusted, err := registry.IsConfigTrusted(ctx, "srv", "cfg-1"); err != nil || !trusted {
				t.Fatalf("approved config untrusted: %v, %v", trusted, err)
			}
			if trusted, err := registry.IsConfigTrusted(ctx, "srv", "cfg-2"); err != nil || trusted {
				t.Fatalf("different config digest trusted: %v, %v", trusted, err)
			}
			if trusted, err := registry.IsConfigTrusted(ctx, "srv", ""); err != nil || trusted {
				t.Fatalf("empty digest trusted: %v, %v", trusted, err)
			}
			if trusted, err := registry.IsConfigTrusted(ctx, "other", "cfg-1"); err != nil || trusted {
				t.Fatalf("approval leaked to another server: %v, %v", trusted, err)
			}
		})
	}
}

func TestTrustRegistryConfigChangeClearsToolPin(t *testing.T) {
	for name, factory := range trustRegistryFactories(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			registry := factory()
			if err := registry.ApproveConfig(ctx, "srv", "cfg-1"); err != nil {
				t.Fatalf("ApproveConfig(): %v", err)
			}
			if err := registry.SaveSessionTrust(ctx, "srv", "tools-1", []string{"cap"}, []string{"echo"}); err != nil {
				t.Fatalf("SaveSessionTrust(): %v", err)
			}
			if err := registry.Approve(ctx, "srv", "tools-1"); err != nil {
				t.Fatalf("Approve(): %v", err)
			}
			if err := registry.ApproveConfig(ctx, "srv", "cfg-1"); err != nil {
				t.Fatalf("ApproveConfig(same): %v", err)
			}
			if trusted, err := registry.IsTrusted(ctx, "srv", "tools-1"); err != nil || !trusted {
				t.Fatalf("re-approving the same config dropped the tool pin: %v, %v", trusted, err)
			}
			if err := registry.ApproveConfig(ctx, "srv", "cfg-2"); err != nil {
				t.Fatalf("ApproveConfig(changed): %v", err)
			}
			if trusted, err := registry.IsTrusted(ctx, "srv", "tools-1"); err != nil || trusted {
				t.Fatalf("a changed config kept the old tool pin: %v, %v", trusted, err)
			}
			if trusted, err := registry.IsConfigTrusted(ctx, "srv", "cfg-1"); err != nil || trusted {
				t.Fatalf("a changed config kept the old config approval: %v, %v", trusted, err)
			}
		})
	}
}

func TestTrustRegistryRevoke(t *testing.T) {
	for name, factory := range trustRegistryFactories(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			registry := factory()
			if existed, err := registry.Revoke(ctx, "srv"); err != nil || existed {
				t.Fatalf("Revoke(missing) = %v, %v", existed, err)
			}
			if err := registry.ApproveConfig(ctx, "srv", "cfg-1"); err != nil {
				t.Fatalf("ApproveConfig(): %v", err)
			}
			if existed, err := registry.Revoke(ctx, "srv"); err != nil || !existed {
				t.Fatalf("Revoke() = %v, %v", existed, err)
			}
			if trusted, err := registry.IsConfigTrusted(ctx, "srv", "cfg-1"); err != nil || trusted {
				t.Fatalf("revoked server still trusted: %v, %v", trusted, err)
			}
			if _, err := registry.GetTrust(ctx, "srv"); !errors.Is(err, ErrTrustNotFound) {
				t.Fatalf("GetTrust(revoked) = %v, want not found", err)
			}
		})
	}
}

func TestTrustRegistryRejectsEmptyInputs(t *testing.T) {
	for name, factory := range trustRegistryFactories(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			registry := factory()
			if err := registry.ApproveConfig(ctx, "", "cfg"); err == nil {
				t.Error("empty server name accepted")
			}
			if err := registry.ApproveConfig(ctx, "srv", ""); err == nil {
				t.Error("empty digest accepted")
			}
			if _, err := registry.Revoke(ctx, ""); err == nil {
				t.Error("empty server name accepted by revoke")
			}
		})
	}
}

func TestComputeConfigDigest(t *testing.T) {
	serverCfg := &config.MCPServer{
		Name:         "cfg-server",
		Transport:    "stdio",
		Command:      "/usr/bin/tool",
		Args:         []string{"--flag", "val"},
		Environment:  map[string]string{"ENV_A": "1"},
		Capabilities: []string{"fs.read"},
	}
	digest, err := ComputeConfigDigest(serverCfg)
	if err != nil {
		t.Fatalf("ComputeConfigDigest(): %v", err)
	}
	mutations := map[string]func(c *config.MCPServer){
		"command":      func(c *config.MCPServer) { c.Command = "/usr/bin/other" },
		"args":         func(c *config.MCPServer) { c.Args = []string{"--flag", "changed"} },
		"environment":  func(c *config.MCPServer) { c.Environment = map[string]string{"ENV_A": "2"} },
		"capabilities": func(c *config.MCPServer) { c.Capabilities = []string{"fs.write"} },
		"timeout":      func(c *config.MCPServer) { c.RequestTimeout = config.Duration(time.Second) },
		"url":          func(c *config.MCPServer) { c.URL = "https://example.com/mcp" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			modified := *serverCfg
			mutate(&modified)
			changed, err := ComputeConfigDigest(&modified)
			if err != nil {
				t.Fatal(err)
			}
			if changed == digest {
				t.Fatalf("digest ignores %s", name)
			}
		})
	}
	t.Run("nil config fails", func(t *testing.T) {
		_, err := ComputeConfigDigest(nil)
		if code, ok := CodeOf(err); !ok || code != ErrConfigInvalid {
			t.Fatalf("code = %v, %v", code, ok)
		}
	})
}
