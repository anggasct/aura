package interpreter

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func rewriteFile(t *testing.T, path, content string) {
	t.Helper()
	original := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		original = info.Mode().Perm()
	}
	dir := filepath.Dir(path)
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chmod(dir, dirInfo.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
	}()
	if err := os.Chmod(path, 0o600); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, original); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyReportsEveryTamperedState(t *testing.T) {
	cases := []struct {
		name       string
		tamper     func(t *testing.T, f *fixture)
		wantState  State
		wantReason Reason
	}{
		{name: "untouched", tamper: func(*testing.T, *fixture) {}, wantState: StateInstalled, wantReason: ReasonNone},
		{name: "changed file", tamper: func(t *testing.T, f *fixture) {
			rewriteFile(t, filepath.Join(f.versionDir(), bundleDirName, "lib", "data.txt"), "tampered")
		}, wantState: StateInvalid, wantReason: ReasonDigestMismatch},
		{name: "added file", tamper: func(t *testing.T, f *fixture) {
			rewriteFile(t, filepath.Join(f.versionDir(), bundleDirName, "lib", "extra.txt"), "extra")
		}, wantState: StateInvalid, wantReason: ReasonDigestMismatch},
		{name: "removed file", tamper: func(t *testing.T, f *fixture) {
			dir := filepath.Join(f.versionDir(), bundleDirName, "lib")
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(dir, "data.txt")); err != nil {
				t.Fatal(err)
			}
		}, wantState: StateInvalid, wantReason: ReasonDigestMismatch},
		{name: "changed mode", tamper: func(t *testing.T, f *fixture) {
			if err := os.Chmod(filepath.Join(f.versionDir(), bundleDirName, "lib", "data.txt"), 0o666); err != nil {
				t.Fatal(err)
			}
		}, wantState: StateInvalid, wantReason: ReasonDigestMismatch},
		{name: "retargeted link", tamper: func(t *testing.T, f *fixture) {
			dir := filepath.Join(f.versionDir(), bundleDirName, "bin")
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(dir, "alias")
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../lib/data.txt", link); err != nil {
				t.Fatal(err)
			}
		}, wantState: StateInvalid, wantReason: ReasonDigestMismatch},
		{name: "missing bundle", tamper: func(t *testing.T, f *fixture) {
			version, err := os.OpenRoot(f.versionDir())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = version.Close() }()
			if err := os.Chmod(f.versionDir(), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := removeTree(version, bundleDirName); err != nil {
				t.Fatal(err)
			}
		}, wantState: StateInvalid, wantReason: ReasonBundleMissing},
		{name: "missing digest", tamper: func(t *testing.T, f *fixture) {
			if err := os.Chmod(f.versionDir(), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(f.versionDir(), digestFileName)); err != nil {
				t.Fatal(err)
			}
		}, wantState: StateInvalid, wantReason: ReasonDigestMissing},
		{name: "malformed digest", tamper: func(t *testing.T, f *fixture) {
			rewriteFile(t, filepath.Join(f.versionDir(), digestFileName), "not-a-digest\n")
		}, wantState: StateInvalid, wantReason: ReasonDigestMissing},
		{name: "digest without a newline", tamper: func(t *testing.T, f *fixture) {
			rewriteFile(t, filepath.Join(f.versionDir(), digestFileName), digestOf([]byte("x")))
		}, wantState: StateInvalid, wantReason: ReasonDigestMissing},
		{name: "wrong digest", tamper: func(t *testing.T, f *fixture) {
			rewriteFile(t, filepath.Join(f.versionDir(), digestFileName), digestOf([]byte("x"))+"\n")
		}, wantState: StateInvalid, wantReason: ReasonDigestMismatch},
		{name: "version replaced by a file", tamper: func(t *testing.T, f *fixture) {
			root, err := os.OpenRoot(f.runtimeDir())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			if err := removeTree(root, fixtureVersion); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.versionDir(), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, wantState: StateInvalid, wantReason: ReasonTreeUnreadable},
		{name: "executable gone with a rewritten digest", tamper: func(t *testing.T, f *fixture) {
			dir := filepath.Join(f.versionDir(), bundleDirName, "bin")
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(dir, "tool")); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(dir, "alias")); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0o555); err != nil {
				t.Fatal(err)
			}
			bundle, err := os.OpenRoot(filepath.Join(f.versionDir(), bundleDirName))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = bundle.Close() }()
			runtime, err := f.store.lookup(fixtureRuntime)
			if err != nil {
				t.Fatal(err)
			}
			digest, err := treeDigest(t.Context(), bundle, &runtime, f.store.limits)
			if err != nil {
				t.Fatal(err)
			}
			rewriteFile(t, filepath.Join(f.versionDir(), digestFileName), digest+"\n")
		}, wantState: StateInvalid, wantReason: ReasonExecutableMissing},
		{name: "runtime directory gone", tamper: func(t *testing.T, f *fixture) {
			root, err := os.OpenRoot(filepath.Join(f.dataRoot, runtimesDirName))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			if err := removeTree(root, fixtureRuntime); err != nil {
				t.Fatal(err)
			}
		}, wantState: StateAbsent, wantReason: ReasonNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, buildArchive(t, validEntries()))
			f.installed(t)
			tc.tamper(t, f)
			got := f.verification(t)
			if got.State != tc.wantState || got.Reason != tc.wantReason {
				t.Errorf("verification = %+v, want state %q reason %q", got, tc.wantState, tc.wantReason)
			}
			if got.Verified() != (tc.wantState == StateInstalled) {
				t.Errorf("Verified() = %v for state %q", got.Verified(), got.State)
			}
		})
	}
}

func TestVerifyBeforeAnyInstallIsAbsent(t *testing.T) {
	f := newFixture(t, buildArchive(t, validEntries()))
	if got := f.verification(t); got.State != StateAbsent || got.Reason != ReasonNone {
		t.Fatalf("verification = %+v, want absent", got)
	}
}

func TestVerifyHonoursCancellation(t *testing.T) {
	f := newFixture(t, buildArchive(t, validEntries()))
	f.installed(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.store.Verify(ctx, fixtureRuntime); err == nil {
		t.Fatal("Verify with a cancelled context succeeded")
	}
}

func TestVerifyRefusesAnUnknownRuntime(t *testing.T) {
	f := newFixture(t, buildArchive(t, validEntries()))
	_, err := f.store.Verify(t.Context(), "ruby")
	requireCode(t, err, ErrorCodeRuntimeUnknown)
}

func TestTreeDigestStopsAtItsBounds(t *testing.T) {
	f := newFixture(t, buildArchive(t, validEntries()))
	f.installed(t)
	runtime, err := f.store.lookup(fixtureRuntime)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := os.OpenRoot(filepath.Join(f.versionDir(), bundleDirName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bundle.Close() }()
	cases := []struct {
		name   string
		limits extractLimits
	}{
		{name: "entry count", limits: extractLimits{maxExpanded: 1 << 20, maxEntries: 2}},
		{name: "expanded bytes", limits: extractLimits{maxExpanded: 4, maxEntries: 100}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := treeDigest(t.Context(), bundle, &runtime, tc.limits)
			if reason, ok := asTreeReason(err); !ok || reason != ReasonTreeUnsafe {
				t.Fatalf("treeDigest error = %v, want an unsafe tree", err)
			}
		})
	}
}
