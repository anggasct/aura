package codeexec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

var pngHeader = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00")

const importLimit = 1024

type importFixture struct {
	sink     *memorySink
	importer *importer
	outside  string
	output   string
	origin   provenance
}

func newImportFixture(t *testing.T) *importFixture {
	t.Helper()
	sink := &memorySink{}
	base := t.TempDir()
	output := filepath.Join(base, outputDirName)
	if err := os.Mkdir(output, outputDirMode); err != nil {
		t.Fatal(err)
	}
	origin, err := newProvenance("session-1", "request-1", "python", "3.12.16+20261026")
	if err != nil {
		t.Fatal(err)
	}
	return &importFixture{
		sink: sink, importer: &importer{sink: sink, maxFiles: 3, maxBytes: importLimit},
		outside: base, output: output, origin: origin,
	}
}

func (f *importFixture) write(t *testing.T, name string, content []byte) {
	t.Helper()
	target := filepath.Join(f.output, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(target), outputDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *importFixture) run(t *testing.T, names ...string) []ArtifactEntry {
	t.Helper()
	return f.importer.importAll(t.Context(), f.output, names, &f.origin)
}

func TestImportRefusesEachUnsafeOutput(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *importFixture)
		files []string
		want  Refusal
	}{
		{"absent", func(*testing.T, *importFixture) {}, []string{"missing.txt"}, RefusalAbsent},
		{"symlink to a file in the directory", func(t *testing.T, f *importFixture) {
			f.write(t, "real.txt", []byte("data"))
			mustSymlink(t, "real.txt", filepath.Join(f.output, "link.txt"))
		}, []string{"link.txt"}, RefusalSymlink},
		{"symlink to a file outside the directory", func(t *testing.T, f *importFixture) {
			secret := filepath.Join(f.outside, "secret.txt")
			if err := os.WriteFile(secret, []byte("treasure"), 0o600); err != nil {
				t.Fatal(err)
			}
			mustSymlink(t, secret, filepath.Join(f.output, "link.txt"))
		}, []string{"link.txt"}, RefusalSymlink},
		{"dangling symlink", func(t *testing.T, f *importFixture) {
			mustSymlink(t, "nowhere", filepath.Join(f.output, "link.txt"))
		}, []string{"link.txt"}, RefusalSymlink},
		{"file below a symlinked directory", func(t *testing.T, f *importFixture) {
			if err := os.WriteFile(filepath.Join(f.outside, "inside.txt"), []byte("treasure"), 0o600); err != nil {
				t.Fatal(err)
			}
			mustSymlink(t, f.outside, filepath.Join(f.output, "dir"))
		}, []string{"dir/inside.txt"}, RefusalSymlink},
		{"hard link", func(t *testing.T, f *importFixture) {
			original := filepath.Join(f.outside, "original.txt")
			if err := os.WriteFile(original, []byte("treasure"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(original, filepath.Join(f.output, "linked.txt")); err != nil {
				t.Fatal(err)
			}
		}, []string{"linked.txt"}, RefusalHardLink},
		{"fifo", func(t *testing.T, f *importFixture) {
			if err := syscall.Mkfifo(filepath.Join(f.output, "pipe"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, []string{"pipe"}, RefusalNotRegular},
		{"socket", func(t *testing.T, f *importFixture) {
			t.Chdir(f.output)
			listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", "s")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
		}, []string{"s"}, RefusalNotRegular},
		{"directory", func(t *testing.T, f *importFixture) {
			if err := os.Mkdir(filepath.Join(f.output, "dir"), outputDirMode); err != nil {
				t.Fatal(err)
			}
		}, []string{"dir"}, RefusalNotRegular},
		{"file used as a directory", func(t *testing.T, f *importFixture) {
			f.write(t, "plain", []byte("data"))
		}, []string{"plain/child"}, RefusalNotRegular},
		{"oversized", func(t *testing.T, f *importFixture) {
			f.write(t, "big.bin", bytes.Repeat([]byte("x"), importLimit+1))
		}, []string{"big.bin"}, RefusalTooLarge},
		{"parent traversal", func(t *testing.T, f *importFixture) {
			if err := os.WriteFile(filepath.Join(f.outside, "secret.txt"), []byte("treasure"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, []string{"../secret.txt"}, RefusalInvalidName},
		{"absolute path", func(t *testing.T, f *importFixture) {
			f.write(t, "a.txt", []byte("data"))
		}, []string{filepath.Join(os.TempDir(), "a.txt")}, RefusalInvalidName},
		{"disallowed character", func(*testing.T, *importFixture) {}, []string{"a b.txt"}, RefusalInvalidName},
		{"empty name", func(*testing.T, *importFixture) {}, []string{""}, RefusalInvalidName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newImportFixture(t)
			tc.setup(t, f)
			entries := f.run(t, tc.files...)
			if len(entries) != 1 || entries[0].Refused != tc.want || entries[0].ArtifactID != "" || entries[0].Digest != "" || entries[0].Size != 0 {
				t.Fatalf("entries = %+v, want one entry refused as %s", entries, tc.want)
			}
			if puts := f.sink.recorded(); len(puts) != 0 {
				t.Fatalf("the sink received %d artifacts", len(puts))
			}
		})
	}
}

func TestImportIgnoresUndeclaredFilesAndKeepsOtherEntries(t *testing.T) {
	f := newImportFixture(t)
	f.write(t, "declared.txt", []byte("yes"))
	f.write(t, "undeclared.txt", []byte("no"))
	entries := f.run(t, "missing.txt", "declared.txt")
	if len(entries) != 2 || entries[0].Refused != RefusalAbsent || entries[1].Refused != "" {
		t.Fatalf("entries = %+v", entries)
	}
	puts := f.sink.recorded()
	if len(puts) != 1 || string(puts[0].content) != "yes" {
		t.Fatalf("sink received %+v, want only the declared file", puts)
	}
}

func TestImportRecordsDigestMediaTypeSizeAndProvenance(t *testing.T) {
	f := newImportFixture(t)
	content := []byte("a,b\n1,2\n")
	f.write(t, "nested/dir/table.csv", content)
	entries := f.run(t, "nested/dir/table.csv")
	sum := sha256.Sum256(content)
	if len(entries) != 1 {
		t.Fatalf("entries = %+v", entries)
	}
	entry := entries[0]
	if entry.Name != "nested/dir/table.csv" || entry.Digest != hex.EncodeToString(sum[:]) || entry.Size != int64(len(content)) ||
		entry.MediaType != "text/plain; charset=utf-8" || !strings.HasPrefix(entry.ArtifactID, artifactIDPrefix) || entry.Refused != "" {
		t.Fatalf("entry = %+v", entry)
	}
	puts := f.sink.recorded()
	if len(puts) != 1 {
		t.Fatalf("puts = %d", len(puts))
	}
	meta := puts[0].meta
	if meta.ID != entry.ArtifactID || meta.SessionID != "session-1" || meta.Filename != "table.csv" || meta.MediaType != entry.MediaType {
		t.Fatalf("meta = %+v", meta)
	}
	var provenance map[string]string
	if err := json.Unmarshal(meta.Metadata, &provenance); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"kind": "code_execute_output", "runtime": "python", "runtime_version": "3.12.16+20261026", "request_id": "request-1"}
	if len(provenance) != len(want) {
		t.Fatalf("provenance = %v", provenance)
	}
	for key, value := range want {
		if provenance[key] != value {
			t.Fatalf("provenance[%s] = %q, want %q", key, provenance[key], value)
		}
	}
}

func TestImportSniffsTheMediaTypeFromContentNotFromTheName(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		content []byte
		want    string
	}{
		{"png bytes under a text name", "report.txt", pngHeader, "image/png"},
		{"text under a png name", "image.png", []byte("plain words"), "text/plain; charset=utf-8"},
		{"html under a csv name", "table.csv", []byte("<!DOCTYPE html><html><body>x</body></html>"), "text/html; charset=utf-8"},
		{"binary", "blob.bin", []byte{0x00, 0x01, 0x02, 0xff}, "application/octet-stream"},
		{"empty file", "empty", nil, "text/plain; charset=utf-8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newImportFixture(t)
			f.write(t, tc.file, tc.content)
			entries := f.run(t, tc.file)
			if len(entries) != 1 || entries[0].Refused != "" || entries[0].MediaType != tc.want || entries[0].Size != int64(len(tc.content)) {
				t.Fatalf("entries = %+v, want media type %s", entries, tc.want)
			}
		})
	}
}

func TestImportAcceptsAFileAtTheSizeLimit(t *testing.T) {
	f := newImportFixture(t)
	f.write(t, "limit.bin", bytes.Repeat([]byte("x"), importLimit))
	entries := f.run(t, "limit.bin")
	if len(entries) != 1 || entries[0].Refused != "" || entries[0].Size != importLimit {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestImportRefusesRepeatsAndExcessNamesWithoutDroppingThem(t *testing.T) {
	f := newImportFixture(t)
	for _, name := range []string{"a", "b", "c", "d"} {
		f.write(t, name, []byte(name))
	}
	entries := f.run(t, "a", "b", "a", "c", "d")
	want := []Refusal{"", "", RefusalDuplicate, RefusalTooMany, RefusalTooMany}
	if len(entries) != len(want) {
		t.Fatalf("entries = %+v, want %d", entries, len(want))
	}
	for i, entry := range entries {
		if entry.Refused != want[i] {
			t.Fatalf("entry %d = %+v, want refusal %q", i, entry, want[i])
		}
	}
	if puts := f.sink.recorded(); len(puts) != 2 {
		t.Fatalf("sink received %d artifacts, want 2", len(puts))
	}
}

func TestImportReportsEveryNameWhenTheOutputDirectoryIsGone(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *importFixture)
	}{
		{"removed", func(t *testing.T, f *importFixture) {
			if err := os.RemoveAll(f.output); err != nil {
				t.Fatal(err)
			}
		}},
		{"replaced by a symlink", func(t *testing.T, f *importFixture) {
			if err := os.WriteFile(filepath.Join(f.outside, "a.txt"), []byte("treasure"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(f.output); err != nil {
				t.Fatal(err)
			}
			mustSymlink(t, f.outside, f.output)
		}},
		{"replaced by a file", func(t *testing.T, f *importFixture) {
			if err := os.RemoveAll(f.output); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.output, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newImportFixture(t)
			tc.setup(t, f)
			entries := f.run(t, "a.txt", "b.txt")
			if len(entries) != 2 || entries[0].Refused != RefusalAbsent || entries[1].Refused != RefusalAbsent {
				t.Fatalf("entries = %+v", entries)
			}
			if puts := f.sink.recorded(); len(puts) != 0 {
				t.Fatalf("the sink received %d artifacts", len(puts))
			}
		})
	}
}

func TestImportWithoutDeclaredNamesReturnsNothing(t *testing.T) {
	f := newImportFixture(t)
	f.write(t, "a.txt", []byte("x"))
	if entries := f.run(t); entries != nil {
		t.Fatalf("entries = %+v, want none", entries)
	}
}

func TestImportKeepsEarlierArtifactsWhenTheQuotaRunsOut(t *testing.T) {
	f := newImportFixture(t)
	f.sink.quota = 10
	f.write(t, "first.txt", []byte("12345"))
	f.write(t, "second.txt", []byte("123456789"))
	f.write(t, "third.txt", []byte("123"))
	entries := f.run(t, "first.txt", "second.txt", "third.txt")
	want := []Refusal{"", RefusalQuota, ""}
	for i, entry := range entries {
		if entry.Refused != want[i] {
			t.Fatalf("entry %d = %+v, want refusal %q", i, entry, want[i])
		}
	}
	if puts := f.sink.recorded(); len(puts) != 2 || string(puts[0].content) != "12345" || string(puts[1].content) != "123" {
		t.Fatalf("sink received %+v", puts)
	}
}

func TestImportHidesASinkFailureBehindACode(t *testing.T) {
	f := newImportFixture(t)
	f.sink.err = errors.New("disk path /secret/location exploded")
	f.write(t, "a.txt", []byte("x"))
	entries := f.run(t, "a.txt")
	if len(entries) != 1 || entries[0].Refused != RefusalImport {
		t.Fatalf("entries = %+v", entries)
	}
	encoded, err := json.Marshal(entries)
	if err != nil || strings.Contains(string(encoded), "secret") {
		t.Fatalf("encoded entries leak the failure: %s, %v", encoded, err)
	}
}

func TestImportStopsWhenTheContextIsCancelled(t *testing.T) {
	f := newImportFixture(t)
	f.write(t, "a.txt", []byte("x"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	entries := f.importer.importAll(ctx, f.output, []string{"a.txt"}, &f.origin)
	if len(entries) != 1 || entries[0].Refused != RefusalImport {
		t.Fatalf("entries = %+v", entries)
	}
	if puts := f.sink.recorded(); len(puts) != 0 {
		t.Fatalf("the sink stored %d artifacts after cancellation", len(puts))
	}
}

func TestOpenFlagsRefuseASymlinkWithoutHelpFromTheLstatCheck(t *testing.T) {
	f := newImportFixture(t)
	f.write(t, "real.txt", []byte("data"))
	mustSymlink(t, "real.txt", filepath.Join(f.output, "link.txt"))
	file, err := openOutput(filepath.Join(f.output, "link.txt"))
	if err == nil {
		_ = file.Close()
		t.Fatal("the open followed a symlink")
	}
	if openRefusal(err) != RefusalSymlink {
		t.Fatalf("openRefusal(%v) = %s, want symlink", err, openRefusal(err))
	}
}

func TestOpenFlagsDoNotBlockOnAFifo(t *testing.T) {
	f := newImportFixture(t)
	if err := syscall.Mkfifo(filepath.Join(f.output, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	opened := make(chan error, 1)
	go func() {
		file, err := openOutput(filepath.Join(f.output, "pipe"))
		if err == nil {
			info, statErr := file.Stat()
			_ = file.Close()
			if statErr == nil && refuseInfo(info, importLimit) != RefusalNotRegular {
				err = errors.New("the descriptor check accepted a fifo")
			}
		}
		opened <- err
	}()
	select {
	case err := <-opened:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(holderTimeout):
		t.Fatal("opening a fifo blocked")
	}
}

type stubInfo struct {
	mode fs.FileMode
	size int64
	sys  any
}

func (s stubInfo) Name() string       { return "stub" }
func (s stubInfo) Size() int64        { return s.size }
func (s stubInfo) Mode() fs.FileMode  { return s.mode }
func (s stubInfo) ModTime() time.Time { return time.Time{} }
func (s stubInfo) IsDir() bool        { return s.mode.IsDir() }
func (s stubInfo) Sys() any           { return s.sys }

func TestRefuseInfoJudgesTheOpenDescriptor(t *testing.T) {
	cases := []struct {
		name string
		info stubInfo
		want Refusal
	}{
		{"regular single link", stubInfo{mode: 0o600, size: 1, sys: &syscall.Stat_t{Nlink: 1}}, ""},
		{"character device", stubInfo{mode: fs.ModeDevice | fs.ModeCharDevice | 0o600, sys: &syscall.Stat_t{Nlink: 1}}, RefusalNotRegular},
		{"block device", stubInfo{mode: fs.ModeDevice | 0o600, sys: &syscall.Stat_t{Nlink: 1}}, RefusalNotRegular},
		{"socket", stubInfo{mode: fs.ModeSocket | 0o600, sys: &syscall.Stat_t{Nlink: 1}}, RefusalNotRegular},
		{"named pipe", stubInfo{mode: fs.ModeNamedPipe | 0o600, sys: &syscall.Stat_t{Nlink: 1}}, RefusalNotRegular},
		{"irregular", stubInfo{mode: fs.ModeIrregular | 0o600, sys: &syscall.Stat_t{Nlink: 1}}, RefusalNotRegular},
		{"symlink", stubInfo{mode: fs.ModeSymlink | 0o600, sys: &syscall.Stat_t{Nlink: 1}}, RefusalSymlink},
		{"two links", stubInfo{mode: 0o600, size: 1, sys: &syscall.Stat_t{Nlink: 2}}, RefusalHardLink},
		{"no link count", stubInfo{mode: 0o600, size: 1, sys: nil}, RefusalUnreadable},
		{"above the limit", stubInfo{mode: 0o600, size: importLimit + 1, sys: &syscall.Stat_t{Nlink: 1}}, RefusalTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := refuseInfo(tc.info, importLimit); got != tc.want {
				t.Fatalf("refuseInfo = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestImportQuotaExhaustionLeavesNoPartialArtifactInTheStore(t *testing.T) {
	db := storetest.OpenMigrated(t)
	if err := store.NewSessionService(db).Create(t.Context(), &store.Session{
		ID: "session-1", OwnerID: "owner-1", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	artifacts := store.NewArtifactStore(db, root, 8)
	f := newImportFixture(t)
	f.importer.sink = storeSink{store: artifacts}
	f.write(t, "fits.txt", []byte("12345"))
	f.write(t, "too-big.txt", []byte("1234567890"))
	f.write(t, "over-total.txt", []byte("abcdef"))

	entries := f.run(t, "fits.txt", "too-big.txt", "over-total.txt")
	if entries[0].Refused != "" || entries[1].Refused != RefusalQuota || entries[2].Refused != RefusalQuota {
		t.Fatalf("entries = %+v", entries)
	}
	var blobs, refs int
	if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM blob").Scan(&blobs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM artifact_ref").Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if blobs != 1 || refs != 1 {
		t.Fatalf("blobs = %d, refs = %d, want only the artifact that fit", blobs, refs)
	}
	var files []string
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.Type().IsRegular() {
			files = append(files, path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("blob files on disk = %v, want exactly the artifact that fit", files)
	}
	reader, meta, err := artifacts.Open(t.Context(), entries[0].ArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	var provenance map[string]string
	if err := json.Unmarshal(meta.Metadata, &provenance); err != nil || provenance["kind"] != "code_execute_output" {
		t.Fatalf("stored provenance = %s, %v", meta.Metadata, err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
