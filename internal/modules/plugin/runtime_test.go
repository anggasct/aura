package plugin

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPlanActivatesOnlyApprovedPlugins(t *testing.T) {
	env := newTestEnv(t)
	env.approve("weather", env.install(baseManifest, serverFiles()))
	other := replaceOnce(t, baseManifest, "id: weather", "id: quarantined-one")
	env.install(other, serverFiles())
	plan, err := env.service.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Held) != 0 || len(plan.Active) != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	active := plan.Active[0]
	if active.ID != "weather" || active.Version != "1.0.0" {
		t.Fatalf("active = %+v", active)
	}
	if want := filepath.Join(env.dir, "weather", "1.0.0", "server"); active.Server.Command != want {
		t.Fatalf("command = %q, want %q", active.Server.Command, want)
	}
	if !slices.Equal(active.Server.Capabilities, []string{"network"}) || active.Server.Transport != "stdio" {
		t.Fatalf("server = %+v", active.Server)
	}
	var names []string
	for _, tool := range active.Tools {
		names = append(names, tool.Name+":"+tool.Tier)
	}
	if !slices.Equal(names, []string{"alerts:read", "forecast:read"}) {
		t.Fatalf("tools = %v", names)
	}
}

func TestPlanKeepsATamperedBundleOff(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, bundle string)
	}{
		{"changed file", func(t *testing.T, bundle string) {
			if err := os.WriteFile(filepath.Join(bundle, "server"), []byte("#!/bin/sh\nevil\n"), 0o700); err != nil {
				t.Fatalf("write: %v", err)
			}
		}},
		{"added file", func(t *testing.T, bundle string) { writeFile(t, filepath.Join(bundle, "extra"), "x", 0o600) }},
		{"removed file", func(t *testing.T, bundle string) {
			if err := os.Remove(filepath.Join(bundle, "server")); err != nil {
				t.Fatalf("remove: %v", err)
			}
		}},
		{"changed permissions", func(t *testing.T, bundle string) {
			if err := os.Chmod(filepath.Join(bundle, "server"), 0o600); err != nil {
				t.Fatalf("chmod: %v", err)
			}
		}},
		{"edited manifest", func(t *testing.T, bundle string) {
			writeFile(t, filepath.Join(bundle, ManifestFile), replaceOnce(t, baseManifest, "[--quiet]", "[--evil]"), 0o600)
		}},
		{"symlink planted", func(t *testing.T, bundle string) { mustSymlink(t, "/etc/passwd", filepath.Join(bundle, "link")) }},
		{"bundle removed", func(t *testing.T, bundle string) {
			if err := os.RemoveAll(bundle); err != nil {
				t.Fatalf("remove: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.approve("weather", env.install(baseManifest, serverFiles()))
			tc.mutate(t, filepath.Join(env.dir, "weather", "1.0.0"))
			plan, err := env.service.Plan(t.Context())
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if len(plan.Active) != 0 || len(plan.Held) != 1 || plan.Held[0].Code != ErrTampered || plan.Held[0].ID != "weather" {
				t.Fatalf("plan = %+v, want the plugin held as tampered", plan)
			}
		})
	}
}

func TestPlanDropsToolsWithoutAMatchingGrant(t *testing.T) {
	env := newTestEnv(t)
	env.approve("weather", env.install(baseManifest, serverFiles()))
	env.registry.mu.Lock()
	grants := env.registry.grants["weather"]
	env.registry.grants["weather"] = slices.DeleteFunc(grants, func(g GrantRecord) bool { return g.Surface == SurfaceTool && g.Value == "alerts" })
	for i := range env.registry.grants["weather"] {
		if env.registry.grants["weather"][i].Value == "forecast" {
			env.registry.grants["weather"][i].Fingerprint = "stale"
		}
	}
	env.registry.mu.Unlock()
	plan, err := env.service.Plan(t.Context())
	if err != nil || len(plan.Active) != 1 || len(plan.Active[0].Tools) != 0 {
		t.Fatalf("plan = %+v, %v; want a running plugin with no tools", plan, err)
	}
}

func TestPlanHoldsAPluginWhoseApprovedVersionIsMissing(t *testing.T) {
	env := newTestEnv(t)
	env.approve("weather", env.install(baseManifest, serverFiles()))
	env.registry.mu.Lock()
	record := env.registry.plugins["weather"]
	record.ApprovedVersion = "9.9.9"
	env.registry.plugins["weather"] = record
	env.registry.mu.Unlock()
	plan, err := env.service.Plan(t.Context())
	if err != nil || len(plan.Active) != 0 || len(plan.Held) != 1 || plan.Held[0].Code != ErrSurfacePending {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
}

func TestPlanRequiresTheBackendHostGrantForRemotePlugins(t *testing.T) {
	remote := `api_version: aura.plugin/v1
id: notes
version: 1.0.0
name: Notes
description: Notes.
tools:
  - name: search
    description: Search.
    tier: read
egress: [notes.example.com, cdn.example.com]
auth:
  kind: none
backend:
  kind: mcp
  transport: streamable_http
  url: https://notes.example.com/mcp
`
	env := newTestEnv(t)
	env.approve("notes", env.install(remote, nil))
	plan, err := env.service.Plan(t.Context())
	if err != nil || len(plan.Active) != 1 || plan.Active[0].Server.URL != "https://notes.example.com/mcp" {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	env.registry.mu.Lock()
	env.registry.grants["notes"] = slices.DeleteFunc(env.registry.grants["notes"], func(g GrantRecord) bool { return g.Surface == SurfaceEgress && g.Value == "notes.example.com" })
	env.registry.mu.Unlock()
	plan, err = env.service.Plan(t.Context())
	if err != nil || len(plan.Active) != 0 || len(plan.Held) != 1 || plan.Held[0].Code != ErrSurfacePending {
		t.Fatalf("plan without the host grant = %+v, %v", plan, err)
	}
}
