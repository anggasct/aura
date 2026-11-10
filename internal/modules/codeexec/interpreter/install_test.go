package interpreter

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

var refusalDetails = map[string]string{
	"parent traversal":                         "not a safe relative path",
	"nested traversal":                         "not a safe relative path",
	"absolute root directory":                  "not a safe relative path",
	"absolute name":                            "not a safe relative path",
	"backslash in name":                        "not a safe relative path",
	"control character in name":                "not a safe relative path",
	"overlong component":                       "not a safe relative path",
	"absolute symlink":                         "not a relative path",
	"escaping symlink":                         "outside the archive root",
	"empty symlink target":                     "not a relative path",
	"symlink escaping through another symlink": "outside the archive root",
	"symlink loop":                             "outside the archive root or loops",
	"entry below a symlink":                    "below a file or symbolic link",
	"entry below a file":                       "below a file or symbolic link",
	"duplicate file":                           "used twice",
	"file over a directory":                    "used twice",
	"hard link":                                "hard links",
	"character device":                         "device and fifo",
	"block device":                             "device and fifo",
	"fifo":                                     "device and fifo",
	"contiguous file type":                     "type is not supported",
	"setuid file":                              "setuid, setgid and sticky",
	"setgid file":                              "setuid, setgid and sticky",
	"sticky directory":                         "setuid, setgid and sticky",
	"missing executable":                       "does not contain the pinned",
	"executable is a directory":                "does not contain the pinned",
	"executable without execute bit":           "does not contain the pinned",
	"over the expanded size":                   "expands beyond",
	"more entries than allowed":                "too many entries",
	"compressed stream larger than allowed":    "expands beyond",
	"not gzip":                                 "not a readable gzip",
	"empty file":                               "not a readable gzip",
	"gzip without a tar":                       "not a readable gzip",
	"truncated gzip":                           "not a readable gzip",
	"truncated tar":                            "not a readable gzip",
}

func TestInstallRefusesUnsafeArchives(t *testing.T) {
	smallExpanded := func(s *Store) { s.limits.maxExpanded = 1024 }
	cases := []struct {
		name    string
		entries []tarEntry
		raw     func(t *testing.T) []byte
		limits  func(*Store)
	}{
		{name: "parent traversal", entries: append(validEntries(), fileEntry("../evil", "x", 0o644))},
		{name: "nested traversal", entries: append(validEntries(), fileEntry("lib/../../evil", "x", 0o644))},
		{name: "absolute root directory", entries: append(validEntries(), dirEntry("/"))},
		{name: "absolute name", entries: append(validEntries(), fileEntry("/etc/evil", "x", 0o644))},
		{name: "backslash in name", entries: append(validEntries(), fileEntry("lib\\evil", "x", 0o644))},
		{name: "control character in name", entries: append(validEntries(), fileEntry("lib/bad\x01name", "x", 0o644))},
		{name: "overlong component", entries: append(validEntries(), fileEntry("lib/"+strings.Repeat("a", 300), "x", 0o644))},
		{name: "absolute symlink", entries: append(validEntries(), linkEntry("lib/etc", "/etc/passwd"))},
		{name: "escaping symlink", entries: append(validEntries(), linkEntry("lib/up", "../../outside"))},
		{name: "empty symlink target", entries: append(validEntries(), linkEntry("lib/empty", ""))},
		{name: "symlink escaping through another symlink", entries: append(validEntries(),
			dirEntry("d/"), linkEntry("d/sub", ".."), linkEntry("link", "d/sub/.."))},
		{name: "symlink loop", entries: append(validEntries(), linkEntry("a", "b"), linkEntry("b", "a"))},
		{name: "entry below a symlink", entries: append(validEntries(), linkEntry("shortcut", "lib"), fileEntry("shortcut/evil", "x", 0o644))},
		{name: "entry below a file", entries: append(validEntries(), fileEntry("bin/tool/evil", "x", 0o644))},
		{name: "duplicate file", entries: append(validEntries(), fileEntry("lib/data.txt", "again", 0o644))},
		{name: "file over a directory", entries: append(validEntries(), fileEntry("lib", "x", 0o644))},
		{name: "hard link", entries: append(validEntries(), tarEntry{name: "lib/hard", typeflag: tar.TypeLink, link: "bin/tool", mode: 0o755})},
		{name: "character device", entries: append(validEntries(), tarEntry{name: "lib/null", typeflag: tar.TypeChar, mode: 0o644})},
		{name: "block device", entries: append(validEntries(), tarEntry{name: "lib/disk", typeflag: tar.TypeBlock, mode: 0o644})},
		{name: "fifo", entries: append(validEntries(), tarEntry{name: "lib/pipe", typeflag: tar.TypeFifo, mode: 0o644})},
		{name: "contiguous file type", entries: append(validEntries(), tarEntry{name: "lib/cont", typeflag: tar.TypeCont, mode: 0o644})},
		{name: "setuid file", entries: append(validEntries(), fileEntry("lib/suid", "x", 0o4755))},
		{name: "setgid file", entries: append(validEntries(), fileEntry("lib/sgid", "x", 0o2755))},
		{name: "sticky directory", entries: append(validEntries(), tarEntry{name: "tmp/", typeflag: tar.TypeDir, mode: 0o1777})},
		{name: "missing executable", entries: []tarEntry{dirEntry("lib/"), fileEntry("lib/data.txt", "data", 0o644)}},
		{name: "executable is a directory", entries: []tarEntry{dirEntry("bin/"), dirEntry("bin/tool/")}},
		{name: "executable without execute bit", entries: []tarEntry{dirEntry("bin/"), fileEntry("bin/tool", "x", 0o644)}},
		{name: "over the expanded size", entries: append(validEntries(), fileEntry("lib/big", strings.Repeat("a", 2048), 0o644)), limits: smallExpanded},
		{name: "more entries than allowed", entries: validEntries(), limits: func(s *Store) { s.limits.maxEntries = 3 }},
		{
			name:    "compressed stream larger than allowed",
			entries: append(validEntries(), fileEntry("lib/zeros", strings.Repeat("\x00", 1<<20), 0o644)),
			limits:  func(s *Store) { s.limits.maxStream = 64 << 10 },
		},
		{name: "not gzip", raw: func(*testing.T) []byte { return []byte("plain text, not an archive") }},
		{name: "empty file", raw: func(*testing.T) []byte { return nil }},
		{name: "gzip without a tar", raw: func(t *testing.T) []byte { return gzipBytes(t, []byte("not a tar stream at all")) }},
		{name: "truncated gzip", raw: func(t *testing.T) []byte {
			full := buildArchive(t, validEntries())
			return full[:len(full)/2]
		}},
		{name: "truncated tar", raw: func(t *testing.T) []byte {
			full := buildTar(t, append(validEntries(), fileEntry("lib/big", strings.Repeat("b", 4096), 0o644)))
			return gzipBytes(t, full[:len(full)-3000])
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var archive []byte
			if tc.raw != nil {
				archive = tc.raw(t)
			} else {
				archive = buildArchive(t, tc.entries)
			}
			f := newFixture(t, archive)
			if tc.limits != nil {
				tc.limits(f.store)
			}
			_, err := f.store.Install(t.Context(), fixtureRuntime, f.archivePath)
			requireCode(t, err, ErrorCodeArchiveInvalid)
			if want := refusalDetails[tc.name]; !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to explain %q", err, want)
			}
			if left := f.entries(t); len(left) != 0 {
				t.Errorf("runtime directory holds %v after a refused install", left)
			}
			if got := f.verification(t); got.State != StateAbsent {
				t.Errorf("state after refused install = %+v, want absent", got)
			}
		})
	}
}

func TestInstallRefusesAWrongDigest(t *testing.T) {
	pinned := buildArchive(t, validEntries())
	f := newFixture(t, pinned)
	other := buildArchive(t, append(validEntries(), fileEntry("lib/extra", "x", 0o644)))
	if err := os.WriteFile(f.archivePath, other, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := f.store.Install(t.Context(), fixtureRuntime, f.archivePath)
	requireCode(t, err, ErrorCodeDigestMismatch)
	if left := f.entries(t); len(left) != 0 {
		t.Errorf("runtime directory holds %v after a digest mismatch", left)
	}
}

func TestInstallRefusesBadArguments(t *testing.T) {
	archive := buildArchive(t, validEntries())
	f := newFixture(t, archive)
	dir := t.TempDir()
	cases := []struct {
		name    string
		runtime string
		path    string
		want    ErrorCode
	}{
		{name: "unknown runtime", runtime: "ruby", path: f.archivePath, want: ErrorCodeRuntimeUnknown},
		{name: "path-like runtime", runtime: "../python", path: f.archivePath, want: ErrorCodeRuntimeUnknown},
		{name: "empty archive path", runtime: fixtureRuntime, path: "", want: ErrorCodeInvalidArgument},
		{name: "missing archive", runtime: fixtureRuntime, path: filepath.Join(dir, "missing.tar.gz"), want: ErrorCodeArchiveInvalid},
		{name: "directory instead of archive", runtime: fixtureRuntime, path: dir, want: ErrorCodeArchiveInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.store.Install(t.Context(), tc.runtime, tc.path)
			requireCode(t, err, tc.want)
			if strings.Contains(err.Error(), dir) {
				t.Errorf("error leaks an absolute path: %v", err)
			}
		})
	}
	if left := f.entries(t); len(left) != 0 {
		t.Errorf("runtime directory holds %v after refused installs", left)
	}
}

func TestInstallRefusesAReinstall(t *testing.T) {
	f := newFixture(t, buildArchive(t, validEntries()))
	f.installed(t)
	before := f.verification(t)
	_, err := f.store.Install(t.Context(), fixtureRuntime, f.archivePath)
	requireCode(t, err, ErrorCodeAlreadyInstalled)
	if got := f.verification(t); got != before || got.State != StateInstalled {
		t.Errorf("state after refused reinstall = %+v, want %+v", got, before)
	}
	if left := f.entries(t); len(left) != 1 || left[0] != fixtureVersion {
		t.Errorf("runtime directory = %v, want only the version", left)
	}
}

func TestInstallLeavesAReadOnlyVerifiedTree(t *testing.T) {
	f := newFixture(t, buildArchive(t, validEntries()))
	f.installed(t)
	if got := f.verification(t); got.State != StateInstalled || got.Reason != ReasonNone || got.Version != fixtureVersion {
		t.Fatalf("verification = %+v, want installed", got)
	}
	if left := f.entries(t); len(left) != 1 || left[0] != fixtureVersion {
		t.Fatalf("runtime directory = %v, want only the version", left)
	}
	top, err := os.ReadDir(f.versionDir())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range top {
		names = append(names, entry.Name())
	}
	if strings.Join(names, ",") != bundleDirName+","+digestFileName {
		t.Errorf("version directory holds %v, want the bundle and the digest only", names)
	}
	bundle := filepath.Join(f.versionDir(), bundleDirName)
	wantModes := map[string]fs.FileMode{
		".":            fs.ModeDir | 0o555,
		"bin":          fs.ModeDir | 0o555,
		"lib":          fs.ModeDir | 0o555,
		"bin/tool":     0o555,
		"lib/data.txt": 0o444,
	}
	for name, want := range wantModes {
		info, err := os.Lstat(filepath.Join(bundle, name))
		if err != nil {
			t.Fatalf("lstat %s: %v", name, err)
		}
		if info.Mode() != want {
			t.Errorf("mode of %s = %v, want %v", name, info.Mode(), want)
		}
	}
	target, err := os.Readlink(filepath.Join(bundle, "bin", "alias"))
	if err != nil || target != "tool" {
		t.Errorf("alias target = %q, %v; want tool", target, err)
	}
	info, err := os.Stat(f.versionDir())
	if err != nil || info.Mode().Perm() != 0o555 {
		t.Errorf("version directory mode = %v, %v; want 0555", info.Mode(), err)
	}
}

func TestInstallAcceptsAnArchiveRootedAtDot(t *testing.T) {
	entries := []tarEntry{dirEntry("./"), dirEntry("./bin/"), fileEntry("./bin/tool", "tool-binary", 0o755)}
	f := newFixture(t, buildArchive(t, entries))
	f.installed(t)
	if got := f.verification(t); got.State != StateInstalled {
		t.Fatalf("verification = %+v, want installed", got)
	}
}

func TestInstallCancelledLeavesNothing(t *testing.T) {
	f := newFixture(t, buildArchive(t, validEntries()))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := f.store.Install(ctx, fixtureRuntime, f.archivePath)
	if err == nil {
		t.Fatal("Install with a cancelled context succeeded")
	}
	if left := f.entries(t); len(left) != 0 {
		t.Errorf("runtime directory holds %v after a cancelled install", left)
	}
}

func TestInterruptedInstallLeavesNoRuntimeAndIsSwept(t *testing.T) {
	archive := buildArchive(t, validEntries())
	f := newFixture(t, archive)
	runtime, err := f.store.lookup(fixtureRuntime)
	if err != nil {
		t.Fatal(err)
	}
	runtimesRoot, err := f.store.openRuntimesRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runtimesRoot.Close() }()
	idRoot, err := openRuntimeDir(runtimesRoot, fixtureRuntime)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = idRoot.Close() }()
	staged, err := f.store.stage(t.Context(), idRoot, &runtime, f.archivePath)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if !strings.HasPrefix(staged, tempPrefix) {
		t.Fatalf("staged name %q lacks the temporary prefix", staged)
	}
	if left := f.entries(t); len(left) != 1 || left[0] != staged {
		t.Fatalf("runtime directory = %v, want only the staged directory", left)
	}
	if got := f.verification(t); got.State != StateAbsent {
		t.Fatalf("state with only a staged directory = %+v, want absent", got)
	}
	listed, err := f.store.List(t.Context())
	if err != nil || len(listed) != 1 || listed[0].State != StateAbsent {
		t.Fatalf("List = %+v, %v; want one absent runtime", listed, err)
	}

	f.installed(t)
	if left := f.entries(t); len(left) != 1 || left[0] != fixtureVersion {
		t.Fatalf("runtime directory after the next install = %v, want only the version", left)
	}
	if got := f.verification(t); got.State != StateInstalled {
		t.Fatalf("state after the next install = %+v, want installed", got)
	}
}

func TestRemoveWithoutApplyChangesNothing(t *testing.T) {
	f := newFixture(t, buildArchive(t, validEntries()))
	f.installed(t)
	before := snapshotTree(t, f.dataRoot)
	removal, err := f.store.Remove(t.Context(), fixtureRuntime, false)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if removal.Applied || len(removal.Versions) != 1 || removal.Versions[0] != fixtureVersion {
		t.Errorf("removal = %+v, want a plan for the installed version", removal)
	}
	if after := snapshotTree(t, f.dataRoot); !bytes.Equal([]byte(strings.Join(before, "\n")), []byte(strings.Join(after, "\n"))) {
		t.Errorf("tree changed without --apply\nbefore: %v\nafter: %v", before, after)
	}
	if got := f.verification(t); got.State != StateInstalled {
		t.Errorf("state after a dry run = %+v, want installed", got)
	}
}

func TestRemoveApplyDeletesTheReadOnlyTree(t *testing.T) {
	f := newFixture(t, buildArchive(t, validEntries()))
	f.installed(t)
	if err := os.Mkdir(filepath.Join(f.runtimeDir(), tempPrefix+"leftover"), 0o700); err != nil {
		t.Fatal(err)
	}
	removal, err := f.store.Remove(t.Context(), fixtureRuntime, true)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removal.Applied || removal.Temporary != 1 || len(removal.Versions) != 1 {
		t.Errorf("removal = %+v, want the version and one interrupted install", removal)
	}
	if left := f.entries(t); len(left) != 0 {
		t.Errorf("runtime directory holds %v after removal", left)
	}
	if got := f.verification(t); got.State != StateAbsent {
		t.Errorf("state after removal = %+v, want absent", got)
	}
	f.installed(t)
}

func TestRemoveHandlesMissingAndUnknownRuntimes(t *testing.T) {
	f := newFixture(t, buildArchive(t, validEntries()))
	for _, apply := range []bool{false, true} {
		removal, err := f.store.Remove(t.Context(), fixtureRuntime, apply)
		if err != nil || removal.Applied || len(removal.Versions) != 0 {
			t.Errorf("Remove(apply=%v) with nothing installed = %+v, %v", apply, removal, err)
		}
	}
	_, err := f.store.Remove(t.Context(), "ruby", true)
	requireCode(t, err, ErrorCodeRuntimeUnknown)
}

func snapshotTree(t *testing.T, root string) []string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		lines = append(lines, rel+" "+info.Mode().String())
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return lines
}

func TestInstallRefusesAnArchiveThatIsNotARegularFile(t *testing.T) {
	f := newFixture(t, buildArchive(t, validEntries()))
	link := filepath.Join(t.TempDir(), "alias.tar.gz")
	if err := os.Symlink(f.archivePath, link); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Install(t.Context(), fixtureRuntime, link); err != nil {
		t.Fatalf("Install through a symbolic link to the archive: %v", err)
	}
	fifo := filepath.Join(t.TempDir(), "pipe.tar.gz")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	other := newFixture(t, buildArchive(t, validEntries()))
	_, err := other.store.Install(t.Context(), fixtureRuntime, fifo)
	requireCode(t, err, ErrorCodeArchiveInvalid)
}

func TestIOErrorsCarryTheCauseButNoPath(t *testing.T) {
	cause := &fs.PathError{Op: "open", Path: "/private/location/file", Err: syscall.ENOSPC}
	err := ioError("extract archive", cause)
	requireCode(t, err, ErrorCodeIO)
	if !strings.Contains(err.Error(), "no space left on device") || strings.Contains(err.Error(), "/private/location") {
		t.Errorf("error = %q", err)
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Error("the cause is not reachable through errors.Is")
	}
	linkErr := ioError("move runtime into place", &os.LinkError{Op: "rename", Old: "/private/a", New: "/private/b", Err: syscall.EXDEV})
	if strings.Contains(linkErr.Error(), "/private") {
		t.Errorf("error leaks a path: %q", linkErr)
	}
}
