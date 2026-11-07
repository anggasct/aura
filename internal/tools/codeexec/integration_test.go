//go:build linux && integration

package codeexec

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/sandbox"
	"github.com/anggasct/aura/internal/tools/interpreter"
)

const archivesEnv = "AURA_INTERPRETER_ARCHIVES"

var (
	//go:embed testdata/workload.py
	workloadPython string
	//go:embed testdata/workload.js
	workloadJavaScript string
	//go:embed testdata/workload.go
	workloadGo string
)

type liveHost struct {
	manifest *interpreter.Manifest
	data     string
	store    *interpreter.Store
	set      *interpreter.VerifiedSet
	runner   *Runner
}

var host *liveHost

func TestMain(m *testing.M) {
	if sandbox.IsChild(os.Args) {
		os.Exit(sandbox.RunChild())
	}
	os.Exit(runLive(m))
}

func runLive(m *testing.M) int {
	archives := os.Getenv(archivesEnv)
	if archives == "" {
		fmt.Fprintf(os.Stderr, "%s must name a directory holding the pinned runtime archives\n", archivesEnv)
		return 1
	}
	if _, err := sandbox.PrepareHost(); err != nil {
		fmt.Fprintln(os.Stderr, "prepare the sandbox host:", err)
		return 1
	}
	data, err := os.MkdirTemp("", "aura-codeexec-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create the data directory:", err)
		return 1
	}
	defer removeTree(data)
	live, err := installRuntimes(archives, data)
	if err != nil {
		fmt.Fprintln(os.Stderr, "install the runtimes:", err)
		return 1
	}
	host = live
	return m.Run()
}

func installRuntimes(archives, data string) (*liveHost, error) {
	manifest, err := interpreter.DefaultManifest()
	if err != nil {
		return nil, err
	}
	store, err := interpreter.NewStore(data, manifest)
	if err != nil {
		return nil, err
	}
	for _, id := range []string{"python", "javascript", "go"} {
		runtime, _ := manifest.Lookup(id)
		if _, err := store.Install(context.Background(), id, filepath.Join(archives, runtime.Archive)); err != nil {
			return nil, fmt.Errorf("%s: %w", id, err)
		}
	}
	live := &liveHost{manifest: manifest, data: data, store: store}
	if err := live.verify(); err != nil {
		return nil, err
	}
	return live, nil
}

func (h *liveHost) verify() error {
	set, err := interpreter.NewVerifiedSet(h.store.Manifest())
	if err != nil {
		return err
	}
	runner, err := NewRunner(&Options{Store: h.store, Verified: set, Artifacts: &memorySink{}, RunRoot: filepath.Join(h.data, "runs"), Limits: liveLimits()})
	if err != nil {
		return err
	}
	verifier, err := interpreter.NewVerifier(h.store, set, interpreter.WithProber(runner))
	if err != nil {
		return err
	}
	if err := verifier.Run(context.Background()); err != nil {
		return err
	}
	h.set, h.runner = set, runner
	return nil
}

func liveLimits() Limits {
	return Limits{
		Timeout: 30 * time.Second, BuildTimeout: 60 * time.Second, MaxSourceBytes: 256 << 10, MaxStdinBytes: 1 << 20, MaxStdoutBytes: 64 << 10, MaxStderrBytes: 64 << 10,
		MaxMemoryBytes: 512 << 20, MaxProcesses: 64, MaxRunDirBytes: 256 << 20, MaxFileBytes: 32 << 20, MaxOutputFiles: 8,
	}
}

func (h *liveHost) runnerWith(t *testing.T, change func(*Limits)) *Runner {
	t.Helper()
	limits := liveLimits()
	change(&limits)
	runner, err := NewRunner(&Options{Store: h.store, Verified: h.set, Artifacts: &memorySink{}, RunRoot: filepath.Join(h.data, "runs"), Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func (h *liveHost) execute(t *testing.T, runner *Runner, runtime, source, stdin string) Outcome {
	t.Helper()
	outcome, err := runner.Execute(t.Context(), &Request{Runtime: runtime, Source: source, Stdin: stdin})
	if err != nil {
		t.Fatalf("Execute(%s): %v", runtime, err)
	}
	return outcome
}

func (h *liveHost) leftovers(t *testing.T) []string {
	t.Helper()
	var left []string
	if entries, err := os.ReadDir(filepath.Join(h.data, "runs")); err == nil {
		for _, entry := range entries {
			left = append(left, "run dir "+entry.Name())
		}
	}
	if parent, ok := sandbox.AccountingCgroup(); ok {
		entries, err := os.ReadDir(parent)
		if err != nil {
			t.Fatalf("read the accounting cgroup: %v", err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "aura-sandbox-") {
				left = append(left, "cgroup "+entry.Name())
			}
		}
	}
	bundles := filepath.Join(h.data, "runtimes") + "/"
	processes, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("read /proc: %v", err)
	}
	for _, process := range processes {
		if exe, err := os.Readlink(filepath.Join("/proc", process.Name(), "exe")); err == nil && strings.HasPrefix(exe, bundles) {
			left = append(left, "process "+process.Name())
		}
	}
	return left
}

func TestLiveHello(t *testing.T) {
	cases := []struct {
		runtime string
		source  string
		want    string
	}{
		{"python", "print('hello from python')", "hello from python"},
		{"javascript", "console.log('hello from node')", "hello from node"},
		{"go", "package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"hello from go\") }\n", "hello from go"},
	}
	for _, tc := range cases {
		t.Run(tc.runtime, func(t *testing.T) {
			outcome := host.execute(t, host.runner, tc.runtime, tc.source, "")
			if outcome.Class != ClassOK || strings.TrimSpace(outcome.Stdout) != tc.want {
				t.Fatalf("outcome = %+v", outcome)
			}
		})
	}
}

func TestLiveNonASCIIOutput(t *testing.T) {
	cases := []struct {
		runtime string
		source  string
	}{
		{"python", "print('héllo wörld — 你好 🙂')"},
		{"javascript", "console.log('héllo wörld — 你好 🙂')"},
		{"go", "package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"héllo wörld — 你好 🙂\") }\n"},
	}
	for _, tc := range cases {
		t.Run(tc.runtime, func(t *testing.T) {
			outcome := host.execute(t, host.runner, tc.runtime, tc.source, "")
			if outcome.Class != ClassOK || strings.TrimSpace(outcome.Stdout) != "héllo wörld — 你好 🙂" {
				t.Fatalf("outcome = %+v", outcome)
			}
		})
	}
}

func TestLiveStdin(t *testing.T) {
	cases := []struct {
		runtime string
		source  string
	}{
		{"python", "import sys\nprint(sys.stdin.read().upper())"},
		{"javascript", "process.stdin.on('data', d => process.stdout.write(String(d).toUpperCase()))"},
		{"go", "package main\nimport (\"io\"; \"os\"; \"strings\")\nfunc main() {\n\tdata, _ := io.ReadAll(os.Stdin)\n\tos.Stdout.WriteString(strings.ToUpper(string(data)))\n}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.runtime, func(t *testing.T) {
			outcome := host.execute(t, host.runner, tc.runtime, tc.source, "from stdin")
			if outcome.Class != ClassOK || strings.TrimSpace(outcome.Stdout) != "FROM STDIN" {
				t.Fatalf("outcome = %+v", outcome)
			}
		})
	}
}

func TestLiveSocketAttemptKeepsEarlierOutput(t *testing.T) {
	cases := []struct {
		runtime string
		source  string
		stderr  string
	}{
		{"python", "print('before', flush=False)\nimport socket\ns = socket.socket(socket.AF_INET, socket.SOCK_STREAM)\ns.connect(('127.0.0.1', 9))\nprint('after')", "PermissionError"},
		{"javascript", "console.log('before')\nconst net = require('net')\nconst s = net.connect(9, '127.0.0.1')\ns.on('error', e => { console.log('error ' + e.code); process.exit(3) })", ""},
		{"go", "package main\nimport (\"fmt\"; \"net\"; \"os\")\nfunc main() {\n\tfmt.Println(\"before\")\n\tif _, err := net.Dial(\"tcp\", \"127.0.0.1:9\"); err != nil {\n\t\tfmt.Fprintln(os.Stderr, err)\n\t\tos.Exit(3)\n\t}\n\tfmt.Println(\"after\")\n}\n", "permission denied"},
	}
	for _, tc := range cases {
		t.Run(tc.runtime, func(t *testing.T) {
			outcome := host.execute(t, host.runner, tc.runtime, tc.source, "")
			if outcome.Class == ClassPolicyDenied || outcome.Class == ClassOK || !strings.HasPrefix(outcome.Stdout, "before") {
				t.Fatalf("outcome = %+v", outcome)
			}
			if tc.stderr != "" && !strings.Contains(outcome.Stderr, tc.stderr) {
				t.Fatalf("stderr = %q, want it to name %s", outcome.Stderr, tc.stderr)
			}
			t.Logf("outcome: class=%s exit=%d stdout=%q stderr=%.200q", outcome.Class, outcome.ExitCode, outcome.Stdout, outcome.Stderr)
		})
	}
}

func TestLiveAsyncio(t *testing.T) {
	source := `import asyncio

async def worker(n):
    await asyncio.sleep(0)
    return n * 2

async def main():
    a = await asyncio.gather(*(worker(i) for i in range(5)))
    b = await asyncio.to_thread(sum, a)
    print(a, b)

asyncio.run(main())
`
	outcome := host.execute(t, host.runner, "python", source, "")
	if outcome.Class != ClassOK || strings.TrimSpace(outcome.Stdout) != "[0, 2, 4, 6, 8] 20" {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestLiveHostFilesAreNotReadable(t *testing.T) {
	canaryDir := t.TempDir()
	canary := filepath.Join(canaryDir, "secret.txt")
	if err := os.WriteFile(canary, []byte("treasure"), 0o600); err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	targets := []string{canary, "/etc/passwd", filepath.Join(home, ".config"), "/usr/bin", filepath.Join(host.data, "runtimes")}
	sources := map[string]string{
		"python":     "import os\ntarget = %q\ntry:\n    print(open(target).read() if os.path.isfile(target) else os.listdir(target))\nexcept OSError as e:\n    print('denied', e.errno)\n",
		"javascript": "const fs = require('fs')\nconst target = %q\ntry { console.log(fs.statSync(target).isDirectory() ? fs.readdirSync(target) : fs.readFileSync(target, 'utf8')) } catch (e) { console.log('denied ' + e.code) }\n",
	}
	for _, runtime := range []string{"python", "javascript"} {
		for _, target := range targets {
			t.Run(runtime+"/"+filepath.Base(target), func(t *testing.T) {
				outcome := host.execute(t, host.runner, runtime, fmt.Sprintf(sources[runtime], target), "")
				if outcome.Class != ClassOK || !strings.HasPrefix(strings.TrimSpace(outcome.Stdout), "denied") {
					t.Fatalf("outcome = %+v", outcome)
				}
			})
		}
	}
	t.Run("go/every target in one build", func(t *testing.T) {
		quoted := make([]string, len(targets))
		for i, target := range targets {
			quoted[i] = fmt.Sprintf("%q", target)
		}
		source := "package main\nimport (\"fmt\"; \"os\")\nfunc main() {\n\tfor _, target := range []string{" + strings.Join(quoted, ", ") + "} {\n\t\tvar err error\n" +
			"\t\tif info, statErr := os.Stat(target); statErr == nil && info.IsDir() {\n\t\t\t_, err = os.ReadDir(target)\n\t\t} else {\n\t\t\t_, err = os.ReadFile(target)\n\t\t}\n" +
			"\t\tif err != nil {\n\t\t\tfmt.Println(\"denied\")\n\t\t} else {\n\t\t\tfmt.Println(\"read\", target)\n\t\t}\n\t}\n}\n"
		outcome := host.execute(t, host.runner, "go", source, "")
		if outcome.Class != ClassOK || strings.TrimSpace(outcome.Stdout) != strings.TrimSpace(strings.Repeat("denied\n", len(targets))) {
			t.Fatalf("outcome = %+v", outcome)
		}
	})
}

func TestLiveHostFileMetadataCannotChange(t *testing.T) {
	canary := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(canary, []byte("data"), 0o640); err != nil {
		t.Fatal(err)
	}
	past := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(canary, past, past); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(canary)
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]string{
		"python":     fmt.Sprintf("import os\nt = %q\nfor name, call in (('chmod', lambda: os.chmod(t, 0o777)), ('utime', lambda: os.utime(t, (0, 0))), ('chown', lambda: os.chown(t, 0, 0))):\n    try:\n        call(); print(name, 'changed')\n    except OSError as e:\n        print(name, 'denied', e.errno)\n", canary),
		"javascript": fmt.Sprintf("const fs = require('fs')\nconst t = %q\nfor (const [name, call] of [['chmod', () => fs.chmodSync(t, 0o777)], ['utime', () => fs.utimesSync(t, 0, 0)], ['chown', () => fs.chownSync(t, 0, 0)]]) {\n  try { call(); console.log(name, 'changed') } catch (e) { console.log(name, 'denied', e.code) }\n}\n", canary),
		"go":         fmt.Sprintf("package main\nimport (\"fmt\"; \"os\"; \"time\")\nfunc main() {\n\tt := %q\n\tsteps := []struct {\n\t\tname string\n\t\tcall func() error\n\t}{\n\t\t{\"chmod\", func() error { return os.Chmod(t, 0o777) }},\n\t\t{\"utime\", func() error { return os.Chtimes(t, time.Unix(0, 0), time.Unix(0, 0)) }},\n\t\t{\"chown\", func() error { return os.Chown(t, 0, 0) }},\n\t}\n\tfor _, step := range steps {\n\t\tif step.call() == nil {\n\t\t\tfmt.Println(step.name, \"changed\")\n\t\t} else {\n\t\t\tfmt.Println(step.name, \"denied\")\n\t\t}\n\t}\n}\n", canary),
	}
	for runtime, source := range sources {
		t.Run(runtime, func(t *testing.T) {
			outcome := host.execute(t, host.runner, runtime, source, "")
			if outcome.Class != ClassOK || strings.Contains(outcome.Stdout, "changed") || strings.Count(outcome.Stdout, "denied") != 3 {
				t.Fatalf("outcome = %+v", outcome)
			}
			after, err := os.Stat(canary)
			if err != nil {
				t.Fatal(err)
			}
			if after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) {
				t.Fatalf("metadata changed: %v %v -> %v %v", before.Mode(), before.ModTime(), after.Mode(), after.ModTime())
			}
		})
	}
}

func TestLiveStandardLibraryWorkloads(t *testing.T) {
	cases := []struct {
		runtime string
		source  string
	}{
		{"python", workloadPython},
		{"javascript", workloadJavaScript},
		{"go", workloadGo},
	}
	for _, tc := range cases {
		t.Run(tc.runtime, func(t *testing.T) {
			outcome := host.execute(t, host.runner, tc.runtime, tc.source, "")
			if outcome.Class != ClassOK {
				t.Fatalf("outcome = %+v", outcome)
			}
		})
	}
}

func TestLiveGoModuleImportFailsOffline(t *testing.T) {
	source := "package main\nimport (\"fmt\"; \"github.com/google/uuid\")\nfunc main() { fmt.Println(uuid.New()) }\n"
	outcome := host.execute(t, host.runner, "go", source, "")
	if outcome.Class != ClassExitNonzero || outcome.ExitCode != 1 || outcome.Stdout != "" {
		t.Fatalf("outcome = %+v", outcome)
	}
	if !strings.Contains(outcome.Stderr, "GOPROXY=off") {
		t.Fatalf("stderr = %q, want the offline proxy to be the reason", outcome.Stderr)
	}
	for _, network := range []string{"dial tcp", "no such host", "connection refused"} {
		if strings.Contains(outcome.Stderr, network) {
			t.Fatalf("stderr = %q, the toolchain must not reach for the network", outcome.Stderr)
		}
	}
	host.waitClean(t)
}

func TestLiveGoProgramRunsWithAMinimalEnvironment(t *testing.T) {
	bundle, err := host.store.Bundle("go")
	if err != nil {
		t.Fatal(err)
	}
	source := "package main\nimport (\"fmt\"; \"os\"; \"os/exec\"; \"sort\"; \"strings\")\n" +
		"func main() {\n\tentries := os.Environ()\n\tsort.Strings(entries)\n" +
		"\tfmt.Println(strings.Join(entries, \"\\n\"))\n\tdir, _ := os.Getwd()\n\tfmt.Println(dir)\n" +
		fmt.Sprintf("\t_, readErr := os.ReadDir(%q)\n", bundle.Dir) +
		"\t_, lookErr := exec.LookPath(\"go\")\n\tfmt.Println(readErr != nil, lookErr != nil)\n}\n"
	outcome := host.execute(t, host.runner, "go", source, "")
	if outcome.Class != ClassOK {
		t.Fatalf("outcome = %+v", outcome)
	}
	lines := strings.Split(strings.TrimSpace(outcome.Stdout), "\n")
	if len(lines) != 5 {
		t.Fatalf("stdout = %q, want three variables, the working directory and the toolchain probe", outcome.Stdout)
	}
	if lines[4] != "true true" {
		t.Fatalf("toolchain probe = %q, the program must reach neither the bundle nor a go command", lines[4])
	}
	dir := lines[3]
	if !strings.HasPrefix(dir, filepath.Join(host.data, "runs")+"/") {
		t.Fatalf("working directory = %q", dir)
	}
	want := []string{"HOME=" + dir, "LANG=C.UTF-8", "TMPDIR=" + dir}
	if !slices.Equal(lines[:3], want) {
		t.Fatalf("environment = %v, want %v", lines[:3], want)
	}
	host.waitClean(t)
}
