package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/codeexec/interpreter"
	"github.com/anggasct/aura/internal/platform/capability"
	"github.com/anggasct/aura/internal/platform/health"
)

type interpreterRig struct {
	configPath  string
	dataRoot    string
	archivePath string
	manifest    *interpreter.Manifest
	deps        interpreterDeps
}

func interpreterArchive(t *testing.T) []byte {
	t.Helper()
	var raw bytes.Buffer
	writer := tar.NewWriter(&raw)
	for _, entry := range []struct {
		name string
		body string
		dir  bool
		mode int64
	}{
		{name: "bin/", dir: true, mode: 0o755},
		{name: "bin/tool", body: "tool-binary", mode: 0o755},
		{name: "lib/data.txt", body: "data", mode: 0o644},
	} {
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
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func interpreterManifestFor(t *testing.T, archive []byte) *interpreter.Manifest {
	t.Helper()
	sum := sha256.Sum256(archive)
	data, err := json.Marshal(map[string]any{"runtimes": []map[string]any{{
		"id": "python", "version": "1.0.0", "archive": "tool-1.0.0.tar.gz", "sha256": hex.EncodeToString(sum[:]),
		"executable": "bin/tool", "argv": []string{"{executable}", "{source}"}, "environment": map[string]string{},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := interpreter.ParseManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func interpreterConfigText(dataRoot string) string {
	return "version: 1\n" +
		"capabilities:\n  enabled: [code-interpreter]\n" +
		"storage:\n  path: " + dataRoot + "\n" +
		"tools:\n  workspace: " + dataRoot + "/workspace\n" +
		"skills:\n  roots: [" + dataRoot + "/skills]\n" +
		"context:\n  recent_complete_turns: 5\n" +
		"profile:\n  prompt_version: v1\n" +
		"children:\n  recovery: interrupt\n" +
		"vision: {}\n"
}

func newInterpreterRig(t *testing.T, build capability.Build, dependencies capability.Dependencies) *interpreterRig {
	t.Helper()
	archive := interpreterArchive(t)
	dir := t.TempDir()
	rig := &interpreterRig{
		configPath:  filepath.Join(dir, "config.yaml"),
		dataRoot:    filepath.Join(dir, "data"),
		archivePath: filepath.Join(dir, "tool.tar.gz"),
		manifest:    interpreterManifestFor(t, archive),
	}
	if err := os.WriteFile(rig.configPath, []byte(interpreterConfigText(rig.dataRoot)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rig.archivePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := capability.BuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	rig.deps = interpreterDeps{
		load: func(path string) (config.LoadResult, error) {
			return config.LoadWithOptions(path, config.LoadOptions{Build: build, Registry: registry, Dependencies: dependencies})
		},
		manifest:          func() (*interpreter.Manifest, error) { return rig.manifest, nil },
		platformSupported: func() bool { return true },
	}
	t.Cleanup(func() { makeInterpreterTreeRemovable(t, rig.dataRoot) })
	return rig
}

func newAvailableInterpreterRig(t *testing.T) *interpreterRig {
	t.Helper()
	build, err := capability.NewBuild("exec-linux", []string{"code-interpreter"}, "linux")
	if err != nil {
		t.Fatal(err)
	}
	return newInterpreterRig(t, build, capability.Dependencies{
		capability.DependencyProcessContainment: true, capability.DependencyInterpreterRuntime: true,
	})
}

func makeInterpreterTreeRemovable(t *testing.T, dataRoot string) {
	t.Helper()
	_ = filepath.WalkDir(dataRoot, func(path string, entry os.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
}

func (r *interpreterRig) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newInterpreterCmd(&globalFlags{configPath: r.configPath}, r.deps)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func (r *interpreterRig) install(t *testing.T) {
	t.Helper()
	if _, err := r.run(t, "install", "python", "--archive", r.archivePath); err != nil {
		t.Fatalf("install: %v", err)
	}
}

func (r *interpreterRig) versionDir() string {
	return filepath.Join(r.dataRoot, "runtimes", "python", "1.0.0")
}

func TestInterpreterInstallListRemoveLifecycle(t *testing.T) {
	rig := newAvailableInterpreterRig(t)

	out, err := rig.run(t, "list")
	pinned, _ := rig.manifest.Lookup("python")
	if err != nil || !strings.Contains(out, "python\t1.0.0\tabsent\t"+pinned.Archive+" sha256:"+pinned.SHA256) {
		t.Fatalf("list before install = %q, %v", out, err)
	}
	out, err = rig.run(t, "install", "python", "--archive", rig.archivePath)
	if err != nil || !strings.Contains(out, "installed: python 1.0.0") {
		t.Fatalf("install = %q, %v", out, err)
	}
	out, err = rig.run(t, "list")
	if err != nil || !strings.Contains(out, "python\t1.0.0\tinstalled\n") || strings.Contains(out, pinned.Archive) {
		t.Fatalf("list after install = %q, %v", out, err)
	}
	out, err = rig.run(t, "list", "--json")
	if err != nil {
		t.Fatalf("list --json: %v", err)
	}
	var listed interpreterListOutput
	if err := json.Unmarshal([]byte(out), &listed); err != nil || len(listed.Runtimes) != 1 ||
		listed.Runtimes[0] != (interpreterListRow{Runtime: "python", Version: "1.0.0", State: "installed", Archive: pinned.Archive, SHA256: pinned.SHA256}) {
		t.Fatalf("list --json = %q, %+v, %v", out, listed, err)
	}

	before := snapshotInterpreterTree(t, rig.dataRoot)
	out, err = rig.run(t, "remove", "python")
	if err != nil || !strings.Contains(out, "dry-run: would remove python") || !strings.Contains(out, "--apply") {
		t.Fatalf("remove without --apply = %q, %v", out, err)
	}
	if after := snapshotInterpreterTree(t, rig.dataRoot); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("remove without --apply changed the tree\nbefore: %v\nafter: %v", before, after)
	}

	out, err = rig.run(t, "remove", "python", "--apply")
	if err != nil || !strings.Contains(out, "removed: python") {
		t.Fatalf("remove --apply = %q, %v", out, err)
	}
	out, err = rig.run(t, "list")
	if err != nil || !strings.Contains(out, "python\t1.0.0\tabsent") {
		t.Fatalf("list after remove = %q, %v", out, err)
	}
	out, err = rig.run(t, "remove", "python", "--apply")
	if err != nil || !strings.Contains(out, "nothing to remove") {
		t.Fatalf("second remove = %q, %v", out, err)
	}
}

func snapshotInterpreterTree(t *testing.T, root string) []string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
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
		t.Fatal(err)
	}
	return lines
}

func TestInterpreterInstallReportsCodedFailures(t *testing.T) {
	rig := newAvailableInterpreterRig(t)
	other := append([]byte{}, interpreterArchive(t)...)
	other = append(other, 0)
	otherPath := filepath.Join(t.TempDir(), "other.tar.gz")
	if err := os.WriteFile(otherPath, other, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := rig.run(t, "install", "python", "--archive", otherPath)
	if code, ok := interpreter.CodeOf(err); !ok || code != interpreter.ErrorCodeDigestMismatch {
		t.Fatalf("install with a different archive: CodeOf(%v) = %q, %v", err, code, ok)
	}
	if _, statErr := os.Stat(rig.versionDir()); !os.IsNotExist(statErr) {
		t.Errorf("a refused install left %s: %v", rig.versionDir(), statErr)
	}

	rig.install(t)
	_, err = rig.run(t, "install", "python", "--archive", rig.archivePath)
	if code, ok := interpreter.CodeOf(err); !ok || code != interpreter.ErrorCodeAlreadyInstalled {
		t.Fatalf("second install: CodeOf(%v) = %q, %v", err, code, ok)
	}

	_, err = rig.run(t, "install", "ruby", "--archive", rig.archivePath)
	if code, ok := interpreter.CodeOf(err); !ok || code != interpreter.ErrorCodeRuntimeUnknown {
		t.Fatalf("unknown runtime: CodeOf(%v) = %q, %v", err, code, ok)
	}

	if _, err := rig.run(t, "install", "python"); err == nil {
		t.Fatal("install without --archive succeeded")
	}
}

func TestInterpreterListShowsAnInvalidRuntimeWithItsReason(t *testing.T) {
	rig := newAvailableInterpreterRig(t)
	rig.install(t)
	if err := os.Chmod(rig.versionDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(rig.versionDir(), "tree.sha256")); err != nil {
		t.Fatal(err)
	}
	out, err := rig.run(t, "list")
	if err != nil || !strings.Contains(out, "python\t1.0.0\tinvalid (digest_missing)") {
		t.Fatalf("list = %q, %v", out, err)
	}
}

func TestInterpreterCommandsNeedTheCapability(t *testing.T) {
	contained := capability.Dependencies{capability.DependencyProcessContainment: true}
	cases := []struct {
		name         string
		profile      string
		compiled     []string
		dependencies capability.Dependencies
		platform     bool
		wantCode     capability.ErrorCode
	}{
		{"core build", "core", nil, contained, true, capability.ErrorCodeCapabilityUnavailable},
		{"exec-linux without the capability compiled", "exec-linux", nil, contained, true, capability.ErrorCodeCapabilityUnavailable},
		{"unsupported platform", "exec-linux", []string{"code-interpreter"}, contained, false, capability.ErrorCodeCapabilityUnavailable},
	}
	for _, tc := range cases {
		build, err := capability.NewBuild(tc.profile, tc.compiled, "linux")
		if err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"list"}, {"install", "python", "--archive", "x.tar.gz"}, {"remove", "python", "--apply"}} {
			t.Run(tc.name+"/"+args[0], func(t *testing.T) {
				rig := newInterpreterRig(t, build, tc.dependencies)
				rig.deps.platformSupported = func() bool { return tc.platform }
				_, err := rig.run(t, args...)
				if code, ok := capability.CodeOf(err); !ok || code != tc.wantCode {
					t.Fatalf("CodeOf(%v) = %q, %v; want %q", err, code, ok, tc.wantCode)
				}
				if _, statErr := os.Stat(filepath.Join(rig.dataRoot, "runtimes")); !os.IsNotExist(statErr) {
					t.Errorf("a refused command touched the data directory: %v", statErr)
				}
			})
		}
	}
}

func TestInterpreterCommandsWorkWithoutAContainedSandbox(t *testing.T) {
	build, err := capability.NewBuild("exec-linux", []string{"code-interpreter"}, "linux")
	if err != nil {
		t.Fatal(err)
	}
	rig := newInterpreterRig(t, build, capability.Dependencies{})
	if out, err := rig.run(t, "list"); err != nil || !strings.Contains(out, "python") {
		t.Fatalf("list = %q, %v", out, err)
	}
}

func interpreterFindings(t *testing.T, check health.RegisteredCheck) []health.Finding {
	t.Helper()
	registry, err := health.NewRegistry(check)
	if err != nil {
		t.Fatal(err)
	}
	return registry.Evaluate(t.Context())
}

func TestInterpreterStatusChecksReportEachState(t *testing.T) {
	rig := newAvailableInterpreterRig(t)
	loaded, err := rig.deps.load(rig.configPath)
	if err != nil {
		t.Fatal(err)
	}
	checks, err := interpreterManifestChecks(loaded.Config, rig.manifest)
	if err != nil || len(checks) != 1 {
		t.Fatalf("interpreterManifestChecks = %v, %v", checks, err)
	}
	cases := []struct {
		name       string
		prepare    func(t *testing.T)
		wantCode   string
		wantStatus health.Status
		wantDetail string
	}{
		{"absent", func(*testing.T) {}, "interpreter_runtime_absent", health.StatusDegraded, "aura interpreter install python"},
		{"installed", rig.install, "ok", health.StatusUp, "files match the recorded digest"},
		{"changed file", func(t *testing.T) {
			path := filepath.Join(rig.versionDir(), "bundle", "lib", "data.txt")
			for _, name := range []string{filepath.Dir(path), path} {
				if err := os.Chmod(name, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "interpreter_runtime_invalid", health.StatusDegraded, "digest_mismatch"},
	}
	for _, tc := range cases {
		tc.prepare(t)
		findings := interpreterFindings(t, checks[0])
		if len(findings) != 1 || findings[0].Code != tc.wantCode || findings[0].Status != tc.wantStatus || !strings.Contains(findings[0].Detail, tc.wantDetail) {
			t.Errorf("%s: findings = %+v", tc.name, findings)
		}
	}
}

func TestInterpreterStatusChecksAreRegisteredOnlyForAnEnabledCapability(t *testing.T) {
	rig := newAvailableInterpreterRig(t)
	loaded, err := rig.deps.load(rig.configPath)
	if err != nil {
		t.Fatal(err)
	}
	checks, err := interpreterStatusChecks(loaded.Config, loaded.CapabilityReport)
	if err != nil || len(checks) != 1 || checks[0].ID != health.ComponentInterpreter {
		t.Fatalf("enabled: checks = %+v, %v", checks, err)
	}
	disabled := interpreterConfigText(rig.dataRoot)
	disabled = strings.Replace(disabled, "enabled: [code-interpreter]", "enabled: []", 1)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(disabled), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err = rig.deps.load(path)
	if err != nil {
		t.Fatal(err)
	}
	if checks, err := interpreterStatusChecks(loaded.Config, loaded.CapabilityReport); err != nil || len(checks) != 0 {
		t.Fatalf("disabled: checks = %+v, %v; want none", checks, err)
	}
}

func TestInterpreterRuntimeReportsPendingThenTheVerifiedState(t *testing.T) {
	rig := newAvailableInterpreterRig(t)
	rig.install(t)
	loaded, err := rig.deps.load(rig.configPath)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := newInterpreterRuntime(loaded.Config, rig.manifest)
	if err != nil {
		t.Fatal(err)
	}
	findings := interpreterFindings(t, runtime.check(loaded.Config))
	if len(findings) != 1 || findings[0].Code != "interpreter_runtime_pending" || findings[0].Status != health.StatusUnknown {
		t.Fatalf("before verification: findings = %+v", findings)
	}
	runtime.run(t.Context(), slog.New(slog.DiscardHandler))
	findings = interpreterFindings(t, runtime.check(loaded.Config))
	if len(findings) != 1 || findings[0].Code != "ok" || !runtime.set.Verified("python") {
		t.Fatalf("after verification: findings = %+v", findings)
	}
}

func TestInterpreterVerificationRunsInTheBackgroundAndStopsWithItsContext(t *testing.T) {
	rig := newAvailableInterpreterRig(t)
	rig.install(t)
	loaded, err := rig.deps.load(rig.configPath)
	if err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		runtime, err := newInterpreterRuntime(loaded.Config, rig.manifest)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		var tasks sync.WaitGroup
		tasks.Go(func() { runtime.run(ctx, slog.New(slog.DiscardHandler)) })
		tasks.Wait()
		cancel()
		if !runtime.set.Verified("python") {
			t.Fatalf("statuses = %+v, want python verified", runtime.set.Statuses())
		}
	})
}

func TestInterpreterVerificationStopsWhenTheServerStops(t *testing.T) {
	rig := newAvailableInterpreterRig(t)
	rig.install(t)
	loaded, err := rig.deps.load(rig.configPath)
	if err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		runtime, err := newInterpreterRuntime(loaded.Config, rig.manifest)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		var tasks sync.WaitGroup
		tasks.Go(func() { runtime.run(ctx, slog.New(slog.DiscardHandler)) })
		tasks.Wait()
		if status, _ := runtime.set.Status("python"); status.State != interpreter.StatePending {
			t.Fatalf("status after a stopped run = %+v, want pending", status)
		}
	})
}
