package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func inspectDir(t *testing.T, dir string) *Inspection {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer func() { _ = root.Close() }()
	insp, err := Inspect(root)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	return insp
}

func TestDigestIsDeterministic(t *testing.T) {
	files := map[string]string{"server": "#!/bin/sh\n", "lib/a.txt": "alpha", "lib/deep/b.txt": "beta", "z.txt": "zed"}
	first := inspectDir(t, writeBundle(t, baseManifest, files))
	second := inspectDir(t, writeBundle(t, baseManifest, files))
	if first.Digest != second.Digest || first.BundleSHA256 != second.BundleSHA256 {
		t.Fatalf("digests differ: %s vs %s", first.Digest, second.Digest)
	}
	touched := writeBundle(t, baseManifest, files)
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(touched, "lib", "a.txt"), old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if third := inspectDir(t, touched); third.Digest != first.Digest {
		t.Fatal("modification times change the digest")
	}
}

func TestDigestIgnoresManifestSpellingButNotMeaning(t *testing.T) {
	base := inspectDir(t, writeBundle(t, baseManifest, serverFiles()))
	reordered := `api_version: aura.plugin/v1
backend:
  args: [--quiet]
  command: ./server
  transport: stdio
  kind: mcp
auth:
  kind: none
egress: [api.weather.example]
tools:
  - {name: alerts, description: List active alerts., tier: read}
  - {name: forecast, description: Get a forecast for a city., tier: read}
capabilities: [network]
description: Weather forecasts.
name: Weather
version: 1.0.0
id: weather
`
	if got := inspectDir(t, writeBundle(t, reordered, serverFiles())); got.Digest != base.Digest {
		t.Fatalf("reordered manifest changes the digest: %s vs %s", got.Digest, base.Digest)
	}
	spelled := replaceOnce(t, baseManifest, "  args: [--quiet]", "  args: [--quiet]\n  request_timeout: 90s")
	other := replaceOnce(t, baseManifest, "  args: [--quiet]", "  args: [--quiet]\n  request_timeout: 1m30s")
	if inspectDir(t, writeBundle(t, spelled, serverFiles())).Digest != inspectDir(t, writeBundle(t, other, serverFiles())).Digest {
		t.Fatal("equal durations spelled differently change the digest")
	}
}

func TestDigestChangesWithAnythingTheBundleRuns(t *testing.T) {
	base := map[string]string{"server": "#!/bin/sh\n", "lib/a.txt": "alpha"}
	baseDigest := inspectDir(t, writeBundle(t, baseManifest, base)).Digest
	cases := []struct {
		name     string
		manifest string
		files    map[string]string
		mutate   func(t *testing.T, dir string)
	}{
		{name: "file content", manifest: baseManifest, files: map[string]string{"server": "#!/bin/sh\n", "lib/a.txt": "alphb"}},
		{name: "extra file", manifest: baseManifest, files: map[string]string{"server": "#!/bin/sh\n", "lib/a.txt": "alpha", "lib/b.txt": ""}},
		{name: "missing file", manifest: baseManifest, files: map[string]string{"server": "#!/bin/sh\n"}},
		{name: "renamed file", manifest: baseManifest, files: map[string]string{"server": "#!/bin/sh\n", "lib/b.txt": "alpha"}},
		{name: "moved file", manifest: baseManifest, files: map[string]string{"server": "#!/bin/sh\n", "a.txt": "alpha"}},
		{name: "executable bit", manifest: baseManifest, files: base, mutate: func(t *testing.T, dir string) {
			if err := os.Chmod(filepath.Join(dir, "lib", "a.txt"), 0o700); err != nil {
				t.Fatalf("chmod: %v", err)
			}
		}},
		{name: "tool description", manifest: replaceOnce(t, baseManifest, "List active alerts.", "List all alerts."), files: base},
		{name: "tool tier", manifest: replaceOnce(t, baseManifest, "tier: read\negress", "tier: effect\negress"), files: base},
		{name: "version", manifest: replaceOnce(t, baseManifest, "version: 1.0.0", "version: 1.0.1"), files: base},
		{name: "egress", manifest: replaceOnce(t, baseManifest, "[api.weather.example]", "[api.weather.example, other.example]"), files: base},
		{name: "args", manifest: replaceOnce(t, baseManifest, "[--quiet]", "[--verbose]"), files: base},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeBundle(t, tc.manifest, tc.files)
			if tc.mutate != nil {
				tc.mutate(t, dir)
			}
			if got := inspectDir(t, dir).Digest; got == baseDigest {
				t.Fatalf("digest did not change: %s", got)
			}
		})
	}
}

func TestDigestIsHex(t *testing.T) {
	digest := inspectDir(t, writeBundle(t, baseManifest, serverFiles())).Digest
	if len(digest) != DigestHexLength || strings.Trim(digest, "0123456789abcdef") != "" {
		t.Fatalf("digest = %q", digest)
	}
}
