package codeexec

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/sandbox"
	"github.com/anggasct/aura/internal/tools/interpreter"
)

const (
	rigPythonVersion = "3.12.16+20261026"
	rigGoVersion     = "1.27.3"
)

type stubCall struct {
	spec       sandbox.Spec
	command    string
	args       []string
	source     string
	sourceMode fs.FileMode
	sourceName string
}

type stubSandbox struct {
	mu     sync.Mutex
	calls  []stubCall
	result sandbox.Result
	err    error
	before func(ctx context.Context, spec *sandbox.Spec)
}

func (s *stubSandbox) run(ctx context.Context, spec *sandbox.Spec, command string, args ...string) (sandbox.Result, error) {
	call := stubCall{spec: *spec, command: command, args: append([]string(nil), args...)}
	if len(args) > 0 {
		call.sourceName = args[len(args)-1]
		if data, err := os.ReadFile(filepath.Join(spec.WorkingDir, call.sourceName)); err == nil {
			call.source = string(data)
		}
		if info, err := os.Stat(filepath.Join(spec.WorkingDir, call.sourceName)); err == nil {
			call.sourceMode = info.Mode().Perm()
		}
	}
	s.mu.Lock()
	s.calls = append(s.calls, call)
	hook, result, err := s.before, s.result, s.err
	s.mu.Unlock()
	if hook != nil {
		hook(ctx, spec)
	}
	return result, err
}

func (s *stubSandbox) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *stubSandbox) last(t *testing.T) stubCall {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		t.Fatal("the sandbox was never called")
	}
	return s.calls[len(s.calls)-1]
}

type rig struct {
	runner  *Runner
	stub    *stubSandbox
	sink    *memorySink
	data    string
	runRoot string
	store   *interpreter.Store
	set     *interpreter.VerifiedSet
}

func rigLimits() Limits {
	return Limits{
		Timeout: 10 * time.Second, MaxSourceBytes: 1 << 10, MaxStdinBytes: 1 << 10, MaxStdoutBytes: 1 << 10, MaxStderrBytes: 1 << 10,
		MaxMemoryBytes: 256 << 20, MaxProcesses: 32, MaxRunDirBytes: 8 << 20, MaxFileBytes: 4 << 20, MaxOutputFiles: 4,
	}
}

func rigArchive(t testing.TB, label string) []byte {
	t.Helper()
	var raw bytes.Buffer
	compressed := gzip.NewWriter(&raw)
	writer := tar.NewWriter(compressed)
	entries := []struct {
		name string
		body string
		dir  bool
		mode int64
	}{
		{name: "bin/", dir: true, mode: 0o755},
		{name: "bin/tool", body: "binary " + label, mode: 0o755},
	}
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Mode: entry.mode, Typeflag: tar.TypeReg, Size: int64(len(entry.body))}
		if entry.dir {
			header.Typeflag, header.Size = tar.TypeDir, 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

type rigRuntime struct {
	id      string
	version string
	install bool
	env     map[string]string
}

func rigManifest(t testing.TB, archives map[string][]byte, runtimes []rigRuntime) *interpreter.Manifest {
	t.Helper()
	type entry struct {
		ID          string            `json:"id"`
		Version     string            `json:"version"`
		Archive     string            `json:"archive"`
		SHA256      string            `json:"sha256"`
		Executable  string            `json:"executable"`
		Argv        []string          `json:"argv"`
		Environment map[string]string `json:"environment,omitempty"`
	}
	var file struct {
		Runtimes []entry `json:"runtimes"`
	}
	for _, runtime := range runtimes {
		sum := sha256.Sum256(archives[runtime.id])
		file.Runtimes = append(file.Runtimes, entry{
			ID: runtime.id, Version: runtime.version, Archive: runtime.id + ".tar.gz", SHA256: hex.EncodeToString(sum[:]),
			Executable: "bin/tool", Argv: []string{interpreter.PlaceholderExecutable, "-flag", interpreter.PlaceholderSource},
			Environment: runtime.env,
		})
	}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := interpreter.ParseManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func newRig(t *testing.T, change func(*Limits)) *rig {
	t.Helper()
	return newRigWith(t, change, []rigRuntime{
		{id: "python", version: rigPythonVersion, install: true},
		{id: "javascript", version: "26.13.0", install: true, env: map[string]string{"OPENSSL_CONF": "/dev/null"}},
		{id: "go", version: rigGoVersion, install: true},
	})
}

func newRigWith(t *testing.T, change func(*Limits), runtimes []rigRuntime) *rig {
	t.Helper()
	archives := map[string][]byte{}
	paths := map[string]string{}
	for _, runtime := range runtimes {
		archives[runtime.id] = rigArchive(t, runtime.id)
		paths[runtime.id] = filepath.Join(t.TempDir(), runtime.id+".tar.gz")
		if err := os.WriteFile(paths[runtime.id], archives[runtime.id], 0o600); err != nil {
			t.Fatal(err)
		}
	}
	data := t.TempDir()
	t.Cleanup(func() { removeTree(data) })
	store, err := interpreter.NewStore(data, rigManifest(t, archives, runtimes))
	if err != nil {
		t.Fatal(err)
	}
	for _, runtime := range runtimes {
		if !runtime.install {
			continue
		}
		if _, err := store.Install(t.Context(), runtime.id, paths[runtime.id]); err != nil {
			t.Fatalf("install %s: %v", runtime.id, err)
		}
	}
	set, err := interpreter.NewVerifiedSet(store.Manifest())
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := interpreter.NewVerifier(store, set)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	limits := rigLimits()
	if change != nil {
		change(&limits)
	}
	runRoot := filepath.Join(data, "runs")
	sink := &memorySink{}
	runner, err := NewRunner(&Options{Store: store, Verified: set, Artifacts: sink, RunRoot: runRoot, Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	stub := &stubSandbox{}
	runner.sandbox = stub.run
	return &rig{runner: runner, stub: stub, sink: sink, data: data, runRoot: runRoot, store: store, set: set}
}

func (r *rig) runDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(r.runRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func removeTree(root string) {
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
	_ = os.RemoveAll(root)
}
