package plugin

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestInstallLeavesANewPluginQuarantined(t *testing.T) {
	env := newTestEnv(t)
	result := env.install(baseManifest, serverFiles())
	if result.State != StateQuarantined || result.ID != "weather" || result.Version != "1.0.0" || len(result.Digest) != DigestHexLength || result.Unchanged {
		t.Fatalf("result = %+v", result)
	}
	record, err := env.registry.Get(t.Context(), "weather")
	if err != nil || record.State != StateQuarantined || record.ApprovedVersion != "" {
		t.Fatalf("record = %+v, %v", record, err)
	}
	if grants, _ := env.registry.Grants(t.Context(), "weather"); len(grants) != 0 {
		t.Fatalf("grants = %+v", grants)
	}
}

func TestInstallCopiesWithOwnerOnlyModes(t *testing.T) {
	env := newTestEnv(t)
	env.install(baseManifest, map[string]string{"server": "#!/bin/sh\n", "lib/data.txt": "data"})
	bundle := filepath.Join(env.dir, "weather", "1.0.0")
	cases := map[string]os.FileMode{
		".":            0o700,
		"lib":          0o700,
		"lib/data.txt": 0o600,
		ManifestFile:   0o600,
		"server":       0o700,
	}
	for rel, want := range cases {
		info, err := os.Stat(filepath.Join(bundle, rel))
		if err != nil {
			t.Fatalf("Stat %s: %v", rel, err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode = %o, want %o", rel, info.Mode().Perm(), want)
		}
	}
	if info, err := os.Stat(env.dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("plugins dir = %v, %v", info, err)
	}
	if entries := listDir(t, env.dir); len(entries) != 1 || entries[0] != "weather" {
		t.Errorf("plugins dir holds %v", entries)
	}
}

func TestInstallRunsNothingFromThePlugin(t *testing.T) {
	env := newTestEnv(t)
	marker := filepath.Join(t.TempDir(), "ran")
	script := "#!/bin/sh\ntouch " + marker + "\n"
	env.install(baseManifest, map[string]string{"server": script, "install.sh": script, "setup.py": "open('" + marker + "','w')"})
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("plugin code ran during install: %v", err)
	}
}

func TestInstallStopsBeforeAnyCopyOnABadManifest(t *testing.T) {
	cases := []struct {
		name     string
		manifest string
		want     ErrorCode
	}{
		{"parser error", "a: [", ErrManifestInvalid},
		{"schema error", replaceOnce(t, baseManifest, "id: weather", "id: ../weather"), ErrManifestInvalid},
		{"oversize", baseManifest + "# " + strings.Repeat("x", MaxManifestBytes) + "\n", ErrManifestTooLarge},
		{"unsupported backend", replaceOnce(t, baseManifest, "kind: mcp", "kind: script"), ErrBackendUnsupported},
		{"missing manifest", "", ErrManifestInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			_, err := env.service.Install(t.Context(), writeBundle(t, tc.manifest, serverFiles()))
			if !hasCode(err, tc.want) {
				t.Fatalf("Install error = %v, want %s", err, tc.want)
			}
			if entries := listDir(t, env.dir); len(entries) != 0 {
				t.Fatalf("plugins dir holds %v after a rejected manifest", entries)
			}
			if records, _ := env.registry.List(t.Context()); len(records) != 0 {
				t.Fatalf("registry holds %v", records)
			}
		})
	}
}

func TestInstallRejectsUnsafeBundles(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret"), "outside", 0o600)
	cases := []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  ErrorCode
	}{
		{"symlink to a file", func(t *testing.T, dir string) {
			mustSymlink(t, filepath.Join(outside, "secret"), filepath.Join(dir, "link"))
		}, ErrPathInvalid},
		{"symlink to a directory", func(t *testing.T, dir string) { mustSymlink(t, outside, filepath.Join(dir, "linkdir")) }, ErrPathInvalid},
		{"symlink inside a subdirectory", func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "lib", "deep", "ok.txt"), "ok", 0o600)
			mustSymlink(t, filepath.Join(outside, "secret"), filepath.Join(dir, "lib", "deep", "link"))
		}, ErrPathInvalid},
		{"symlink to a sibling inside the bundle", func(t *testing.T, dir string) { mustSymlink(t, "server", filepath.Join(dir, "alias")) }, ErrPathInvalid},
		{"relative symlink escaping with dotdot", func(t *testing.T, dir string) {
			mustSymlink(t, "../../../../../../etc/passwd", filepath.Join(dir, "passwd"))
		}, ErrPathInvalid},
		{"dangling symlink", func(t *testing.T, dir string) { mustSymlink(t, "missing", filepath.Join(dir, "dangling")) }, ErrPathInvalid},
		{"manifest as a symlink", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, ManifestFile)); err != nil {
				t.Fatalf("remove: %v", err)
			}
			writeFile(t, filepath.Join(outside, "real.yaml"), baseManifest, 0o600)
			mustSymlink(t, filepath.Join(outside, "real.yaml"), filepath.Join(dir, ManifestFile))
		}, ErrPathInvalid},
		{"file name with a newline", func(t *testing.T, dir string) { writeFile(t, dir+"/a\nb", "x", 0o600) }, ErrPathInvalid},
		{"file name with a backslash", func(t *testing.T, dir string) { writeFile(t, dir+"/a\\b", "x", 0o600) }, ErrPathInvalid},
		{"too many entries", func(t *testing.T, dir string) {
			for i := range MaxBundleEntries + 1 {
				writeFile(t, filepath.Join(dir, "files", strconv.Itoa(i)), "", 0o600)
			}
		}, ErrBundleTooLarge},
		{"file over the size limit", func(t *testing.T, dir string) {
			file, err := os.Create(filepath.Join(dir, "big"))
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if err := file.Truncate(MaxBundleFileBytes + 1); err != nil {
				t.Fatalf("truncate: %v", err)
			}
			if err := file.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
		}, ErrBundleTooLarge},
		{"nested too deep", func(t *testing.T, dir string) {
			deep := dir
			for range maxBundleDepth + 2 {
				deep = filepath.Join(deep, "d")
			}
			writeFile(t, filepath.Join(deep, "f"), "x", 0o600)
		}, ErrBundleTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			source := writeBundle(t, baseManifest, serverFiles())
			tc.setup(t, source)
			_, err := env.service.Install(t.Context(), source)
			if !hasCode(err, tc.want) {
				t.Fatalf("Install error = %v, want %s", err, tc.want)
			}
			if records, _ := env.registry.List(t.Context()); len(records) != 0 {
				t.Fatalf("registry holds %v", records)
			}
			for _, entry := range listDir(t, env.dir) {
				t.Errorf("plugins dir kept %q after a rejected bundle", entry)
			}
		})
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
}

func TestInstallRejectsAMissingSourceDirectory(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.service.Install(t.Context(), filepath.Join(t.TempDir(), "missing"))
	if !hasCode(err, ErrPathInvalid) {
		t.Fatalf("Install error = %v", err)
	}
	if strings.Contains(err.Error(), t.TempDir()) {
		t.Fatalf("error names a path: %v", err)
	}
}

func TestInstallVersionConflictAndIdempotency(t *testing.T) {
	env := newTestEnv(t)
	first := env.install(baseManifest, serverFiles())
	again := env.install(baseManifest, serverFiles())
	if !again.Unchanged || again.Digest != first.Digest {
		t.Fatalf("second install = %+v, want unchanged %s", again, first.Digest)
	}
	changed := serverFiles()
	changed["server"] += "# changed\n"
	_, err := env.service.Install(t.Context(), writeBundle(t, baseManifest, changed))
	if !hasCode(err, ErrVersionConflict) {
		t.Fatalf("Install error = %v, want %s", err, ErrVersionConflict)
	}
	row, err := env.registry.Version(t.Context(), "weather", "1.0.0")
	if err != nil || row.Digest != first.Digest {
		t.Fatalf("recorded digest = %v, %v", row.Digest, err)
	}
	if entries := listDir(t, env.dir); len(entries) != 1 {
		t.Fatalf("plugins dir holds %v", entries)
	}
	if _, err := env.service.Review(t.Context(), "weather"); err != nil {
		t.Fatalf("installed bundle changed after a conflicting install: %v", err)
	}
}

func TestInstallReplacesAnUnregisteredLeftoverBundle(t *testing.T) {
	env := newTestEnv(t)
	writeFile(t, filepath.Join(env.dir, "weather", "1.0.0", "stale"), "stale", 0o600)
	env.install(baseManifest, serverFiles())
	if _, err := os.Stat(filepath.Join(env.dir, "weather", "1.0.0", "stale")); !os.IsNotExist(err) {
		t.Fatalf("leftover file survived: %v", err)
	}
}

func TestBundlePathsRejectUnsafeIdentifiers(t *testing.T) {
	cases := []struct{ id, version string }{
		{"../weather", "1.0.0"},
		{"weather", "../1.0.0"},
		{"weather/x", "1.0.0"},
		{"weather", "1.0.0/x"},
		{"/abs", "1.0.0"},
		{"weather", ""},
		{"", "1.0.0"},
		{"weather", ".."},
	}
	for _, tc := range cases {
		if _, err := bundleRel(tc.id, tc.version); !hasCode(err, ErrPathInvalid) {
			t.Errorf("bundleRel(%q, %q) error = %v", tc.id, tc.version, err)
		}
	}
	if rel, err := bundleRel("weather", "1.0.0-rc.1+b5"); err != nil || rel != "weather/1.0.0-rc.1+b5" {
		t.Errorf("bundleRel valid = %q, %v", rel, err)
	}
}

func TestPoisonedRegistryRowsNeverComposeAPath(t *testing.T) {
	env := newTestEnv(t)
	for _, row := range []VersionRecord{
		{ID: "weather", Version: "../../x", Digest: strings.Repeat("a", 64)},
		{ID: "../x", Version: "1.0.0", Digest: strings.Repeat("a", 64)},
	} {
		if err := env.service.verify(mustOpenRoot(t, env), &row); !hasCode(err, ErrPathInvalid) {
			t.Errorf("verify(%+v) error = %v", row, err)
		}
	}
}

func mustOpenRoot(t *testing.T, env *testEnv) *os.Root {
	t.Helper()
	root, err := env.service.openPlugins()
	if err != nil {
		t.Fatalf("openPlugins: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}
