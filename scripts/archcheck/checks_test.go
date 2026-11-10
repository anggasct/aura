package main

import (
	"slices"
	"strings"
	"testing"
)

func TestLayerViolations(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		from, to string
		violates bool
	}{
		{"platform to module", "internal/platform/turn/engine", "internal/modules/skills", true},
		{"platform to channel", "internal/platform/effect", "internal/channels/discord", true},
		{"platform to channel kit", "internal/platform/effect", "internal/channels/kit/access", true},
		{"platform to app", "internal/platform/effect", "internal/app", true},
		{"platform to platform", "internal/platform/effect", "internal/platform/turn", false},
		{"module to other module", "internal/modules/github", "internal/modules/workflow", true},
		{"module to own sub-package", "internal/modules/codeexec", "internal/modules/codeexec/interpreter", false},
		{"module to channel", "internal/modules/github", "internal/channels/webhook", true},
		{"module to channel kit", "internal/modules/memory", "internal/channels/kit/media", true},
		{"module to platform", "internal/modules/memory", "internal/platform/turn", false},
		{"module to app", "internal/modules/memory", "internal/app/config", true},
		{"channel to module", "internal/channels/discord", "internal/modules/memory", true},
		{"channel to other channel", "internal/channels/discord", "internal/channels/telegram", true},
		{"channel to channel kit", "internal/channels/discord", "internal/channels/kit/access", false},
		{"channel to platform", "internal/channels/discord", "internal/platform/turn", false},
		{"channel kit to channel", "internal/channels/kit/access", "internal/channels/discord", true},
		{"channel kit to module", "internal/channels/kit/access", "internal/modules/memory", true},
		{"channel kit to platform", "internal/channels/kit/access", "internal/platform/principal", false},
		{"plugin to wire", "plugins/gmail/connector", "internal/modules/plugin/wire", false},
		{"plugin to platform", "plugins/gmail/connector", "internal/platform/toolbroker", true},
		{"platform to cli", "internal/platform/turn", "internal/cli", true},
		{"cli to app", "internal/cli", "internal/app", false},
		{"cli to platform logging", "internal/cli", "internal/platform/logging", false},
		{"cli to platform turn", "internal/cli", "internal/platform/turn", true},
		{"cli to legacy config", "internal/cli", "internal/config", true},
		{"cli to module", "internal/cli", "internal/modules/memory", true},
		{"legacy source is exempt", "internal/config", "internal/modules/memory", false},
		{"legacy target is exempt", "internal/platform/turn", "internal/store", false},
		{"app is exempt", "internal/app", "internal/modules/memory", false},
		{"cmd is exempt", "cmd/aura", "internal/modules/memory", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := layerViolation(tc.from, tc.to); got != tc.violates {
				t.Fatalf("layerViolation(%q, %q) = %v, want %v", tc.from, tc.to, got, tc.violates)
			}
		})
	}
}

func TestCheckLayersHonoursAllowListAndReportsStaleEntries(t *testing.T) {
	t.Parallel()
	packages := []repoPackage{
		{path: "internal/modules/builtin", imports: []string{"internal/modules/codeexec/interpreter", "internal/modules/fetch", "internal/platform/tools"}},
		{path: "internal/platform/effect", imports: []string{"internal/channels/discord", "internal/channels/discord"}},
	}
	allowed, problems := parseAllow("internal/modules/builtin -> internal/modules/codeexec  # S12\n\ninternal/modules/memory -> internal/modules/profile # S08\n")
	if len(problems) != 0 {
		t.Fatalf("parseAllow problems = %v", problems)
	}
	got := checkLayers(packages, allowed)
	want := []string{
		"layers: internal/modules/builtin imports internal/modules/fetch",
		"layers: internal/platform/effect imports internal/channels/discord",
		"layers.allow:3: stale allow entry internal/modules/memory -> internal/modules/profile",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("checkLayers = %q, want %q", got, want)
	}
}

func TestCheckLayersReportsCliImportingPlatformPackages(t *testing.T) {
	t.Parallel()
	packages := []repoPackage{
		{path: "internal/cli", imports: []string{"internal/app", "internal/platform/turn"}},
	}
	got := checkLayers(packages, nil)
	want := []string{"layers: internal/cli imports internal/platform/turn"}
	if !slices.Equal(got, want) {
		t.Fatalf("checkLayers = %q, want %q", got, want)
	}
}

func TestParseAllowRejectsMalformedEntries(t *testing.T) {
	t.Parallel()
	_, problems := parseAllow("internal/modules/a internal/modules/b\n -> internal/modules/b\n")
	if len(problems) != 2 {
		t.Fatalf("problems = %q, want two malformed entries", problems)
	}
}

func TestCheckLayout(t *testing.T) {
	t.Parallel()
	entries := map[string][]dirEntry{
		"internal":          {{name: "platform", isDir: true}, {name: "modules", isDir: true}, {name: "store", isDir: true}, {name: "tools", isDir: true}, {name: "doc.go"}},
		"internal/modules":  {{name: "memory", isDir: true}, {name: "registry.go"}},
		"internal/platform": {{name: "turn", isDir: true}, {name: "README"}},
	}
	got := checkLayout(entries)
	want := []string{
		"layout: internal/tools is not an allowed top-level directory",
		"layout: file internal/doc.go is outside a package group",
		"layout: internal/modules/registry.go sits directly in a group directory",
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("checkLayout = %q, want %q", got, want)
	}
}

func TestCheckStdNames(t *testing.T) {
	t.Parallel()
	packages := []repoPackage{
		{path: "internal/platform/turn", name: "turn"},
		{path: "internal/modules/sync", name: "sync"},
		{path: "internal/modules/plugin", name: "plugin"},
		{path: "cmd/aura", name: "main"},
	}
	stdNames := map[string]string{"sync": "sync", "plugin": "plugin", "runtime": "runtime"}
	got := checkStdNames(packages, stdNames, map[string]bool{"sync": true, "runtime": true})
	if len(got) != 1 || !strings.Contains(got[0], "internal/modules/sync") {
		t.Fatalf("checkStdNames = %q, want only internal/modules/sync", got)
	}
}
