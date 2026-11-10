package interpreter

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const (
	fixtureRuntime    = "python"
	fixtureVersion    = "1.0.0"
	fixtureExecutable = "bin/tool"
)

type tarEntry struct {
	name     string
	typeflag byte
	body     string
	link     string
	mode     int64
}

func dirEntry(name string) tarEntry {
	return tarEntry{name: name, typeflag: tar.TypeDir, mode: 0o755}
}

func fileEntry(name, body string, mode int64) tarEntry {
	return tarEntry{name: name, typeflag: tar.TypeReg, body: body, mode: mode}
}

func linkEntry(name, target string) tarEntry {
	return tarEntry{name: name, typeflag: tar.TypeSymlink, link: target, mode: 0o777}
}

func validEntries() []tarEntry {
	return []tarEntry{
		dirEntry("bin/"),
		fileEntry("bin/tool", "tool-binary", 0o755),
		linkEntry("bin/alias", "tool"),
		dirEntry("lib/"),
		fileEntry("lib/data.txt", "data", 0o644),
	}
}

func buildTar(t testing.TB, entries []tarEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Typeflag: entry.typeflag, Mode: entry.mode, Linkname: entry.link}
		if entry.typeflag == tar.TypeReg {
			header.Size = int64(len(entry.body))
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatalf("write header %q: %v", entry.name, err)
		}
		if entry.typeflag == tar.TypeReg {
			if _, err := writer.Write([]byte(entry.body)); err != nil {
				t.Fatalf("write body %q: %v", entry.name, err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buffer.Bytes()
}

func gzipBytes(t testing.TB, data []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buffer.Bytes()
}

func buildArchive(t testing.TB, entries []tarEntry) []byte {
	t.Helper()
	return gzipBytes(t, buildTar(t, entries))
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func fixtureManifest(t testing.TB, archive []byte) *Manifest {
	t.Helper()
	data, err := json.Marshal(manifestFile{Runtimes: []Runtime{{
		ID: fixtureRuntime, Version: fixtureVersion, Archive: "tool-1.0.0.tar.gz", SHA256: digestOf(archive),
		Executable: fixtureExecutable, Argv: []string{PlaceholderExecutable, PlaceholderSource},
		Environment: map[string]string{"TOOL_MODE": "offline"},
	}}})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifest, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("parse fixture manifest: %v", err)
	}
	return manifest
}

type fixture struct {
	store       *Store
	dataRoot    string
	archivePath string
}

func newFixture(t testing.TB, archive []byte) *fixture {
	t.Helper()
	dataRoot := t.TempDir()
	store, err := NewStore(dataRoot, fixtureManifest(t, archive))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	archivePath := filepath.Join(t.TempDir(), "tool.tar.gz")
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	t.Cleanup(func() { makeTreeRemovable(t, dataRoot) })
	return &fixture{store: store, dataRoot: dataRoot, archivePath: archivePath}
}

func (f *fixture) runtimeDir() string {
	return filepath.Join(f.dataRoot, runtimesDirName, fixtureRuntime)
}

func (f *fixture) versionDir() string {
	return filepath.Join(f.runtimeDir(), fixtureVersion)
}

func (f *fixture) installed(t testing.TB) {
	t.Helper()
	if _, err := f.store.Install(t.Context(), fixtureRuntime, f.archivePath); err != nil {
		t.Fatalf("Install: %v", err)
	}
}

func (f *fixture) entries(t testing.TB) []string {
	t.Helper()
	entries, err := os.ReadDir(f.runtimeDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read runtime dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func (f *fixture) verification(t testing.TB) Verification {
	t.Helper()
	verification, err := f.store.Verify(t.Context(), fixtureRuntime)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return verification
}

func requireCode(t testing.TB, err error, want ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want %q", want)
	}
	got, ok := CodeOf(err)
	if !ok || got != want {
		t.Fatalf("CodeOf(%v) = %q, %v; want %q", err, got, ok, want)
	}
}

func makeTreeRemovable(t testing.TB, dataRoot string) {
	t.Helper()
	root, err := os.OpenRoot(dataRoot)
	if err != nil {
		t.Errorf("open data root: %v", err)
		return
	}
	defer func() { _ = root.Close() }()
	if err := removeTree(root, runtimesDirName); err != nil {
		t.Errorf("remove runtimes tree: %v", err)
	}
}

func gzipStored(t testing.TB, data []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer, err := gzip.NewWriterLevel(&buffer, gzip.NoCompression)
	if err != nil {
		t.Fatalf("gzip writer: %v", err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buffer.Bytes()
}
