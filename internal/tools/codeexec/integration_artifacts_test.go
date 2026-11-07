//go:build linux && integration

package codeexec

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
	"github.com/anggasct/aura/internal/tools/interpreter"
)

const liveSession = "session-live"

type storedHost struct {
	runner    *Runner
	db        *sql.DB
	artifacts store.ArtifactStore
}

func (h *liveHost) storedRunner(t *testing.T, quota int64, change func(*Limits)) *storedHost {
	t.Helper()
	db := storetest.OpenMigrated(t)
	now := time.Now().UTC()
	if err := store.NewSessionService(db).Create(t.Context(), &store.Session{ID: liveSession, OwnerID: "owner-1", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	artifacts := store.NewArtifactStore(db, t.TempDir(), quota)
	limits := liveLimits()
	if change != nil {
		change(&limits)
	}
	runner, err := NewRunner(&Options{
		Store: h.store, Verified: h.set, Artifacts: storeSink{store: artifacts}, RunRoot: filepath.Join(h.data, "runs"), Limits: limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &storedHost{runner: runner, db: db, artifacts: artifacts}
}

func (s *storedHost) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (s *storedHost) run(t *testing.T, runtime, source string, files ...string) Outcome {
	t.Helper()
	outcome, err := s.runner.Execute(t.Context(), &Request{
		Runtime: runtime, Source: source, SessionID: liveSession, RequestID: "request-live", OutputFiles: files,
	})
	if err != nil {
		t.Fatalf("Execute(%s): %v", runtime, err)
	}
	return outcome
}

func (s *storedHost) read(t *testing.T, id string) ([]byte, store.ArtifactMetadata) {
	t.Helper()
	reader, meta, err := s.artifacts.Open(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return content, meta
}

func writeBytes(runtime, name string, size int) string {
	if runtime == "python" {
		return fmt.Sprintf("open(%q, 'wb').write(b'x' * %d)\n", name, size)
	}
	return fmt.Sprintf("require('fs').writeFileSync(%q, Buffer.alloc(%d, 120))\n", name, size)
}

func TestLiveDeclaredOutputsBecomeArtifacts(t *testing.T) {
	pngHex := hex.EncodeToString(pngHeader)
	sources := map[string]string{
		"python": "import os\nos.makedirs('output/sub')\nopen('output/result.csv', 'w').write('a,b\\n1,2\\n')\n" +
			"open('output/chart.png', 'wb').write(bytes.fromhex('" + pngHex + "'))\n" +
			"open('output/sub/n.txt', 'w').write('nested')\nopen('output/undeclared.txt', 'w').write('nope')\nprint('done')\n",
		"javascript": "const fs = require('fs')\nfs.mkdirSync('output/sub', { recursive: true })\nfs.writeFileSync('output/result.csv', 'a,b\\n1,2\\n')\n" +
			"fs.writeFileSync('output/chart.png', Buffer.from('" + pngHex + "', 'hex'))\n" +
			"fs.writeFileSync('output/sub/n.txt', 'nested')\nfs.writeFileSync('output/undeclared.txt', 'nope')\nconsole.log('done')\n",
	}
	for runtime, source := range sources {
		t.Run(runtime, func(t *testing.T) {
			stored := host.storedRunner(t, store.DefaultArtifactQuotaBytes, nil)
			outcome := stored.run(t, runtime, source, "result.csv", "chart.png", "sub/n.txt", "missing.txt")
			if outcome.Class != ClassOK || len(outcome.Artifacts) != 4 {
				t.Fatalf("outcome = %+v", outcome)
			}
			want := []struct {
				name    string
				content string
				media   string
			}{
				{"result.csv", "a,b\n1,2\n", "text/plain; charset=utf-8"},
				{"chart.png", string(pngHeader), "image/png"},
				{"sub/n.txt", "nested", "text/plain; charset=utf-8"},
			}
			for i, expected := range want {
				entry := outcome.Artifacts[i]
				if entry.Name != expected.name || entry.Refused != "" || entry.MediaType != expected.media || entry.Size != int64(len(expected.content)) {
					t.Fatalf("artifact %d = %+v, want %+v", i, entry, expected)
				}
				content, meta := stored.read(t, entry.ArtifactID)
				if string(content) != expected.content || meta.SessionID != liveSession || meta.MediaType != expected.media {
					t.Fatalf("stored artifact %d = %q %+v", i, content, meta)
				}
				for _, fragment := range []string{`"kind":"code_execute_output"`, `"request_id":"request-live"`, `"runtime":"` + runtime + `"`} {
					if !strings.Contains(string(meta.Metadata), fragment) {
						t.Fatalf("provenance of artifact %d = %s, want %s", i, meta.Metadata, fragment)
					}
				}
			}
			if last := outcome.Artifacts[3]; last.Name != "missing.txt" || last.Refused != RefusalAbsent {
				t.Fatalf("missing file entry = %+v", last)
			}
			if refs := stored.count(t, "artifact_ref"); refs != 3 {
				t.Fatalf("artifact refs = %d, want only the three declared files", refs)
			}
			host.waitClean(t)
		})
	}
}

func TestLiveOutputsFollowTheRunClass(t *testing.T) {
	short := func(l *Limits) { l.Timeout = 2 * time.Second }
	fileLimit := func(l *Limits) { l.MaxFileBytes = 64 << 10 }
	cases := []struct {
		name     string
		runtime  string
		source   string
		change   func(*Limits)
		want     ResultClass
		imported bool
	}{
		{"python exit nonzero", "python", "open('output/a.txt', 'w').write('x')\nimport sys\nsys.exit(3)", nil, ClassExitNonzero, true},
		{"javascript exit nonzero", "javascript", "require('fs').writeFileSync('output/a.txt', 'x')\nprocess.exit(3)", nil, ClassExitNonzero, true},
		{"python exception", "python", "open('output/a.txt', 'w').write('x')\nraise ValueError('x')", nil, ClassExitNonzero, true},
		{"python timeout", "python", "open('output/a.txt', 'w').write('x')\nimport time\ntime.sleep(60)", short, ClassTimeout, false},
		{"javascript timeout", "javascript", "require('fs').writeFileSync('output/a.txt', 'x')\nsetTimeout(() => {}, 60000)", short, ClassTimeout, false},
		{"python seccomp kill", "python", "open('output/a.txt', 'w').write('x')\nimport os\nos.symlink('/etc/passwd', 'output/link')", nil, ClassPolicyDenied, false},
		{"javascript seccomp kill", "javascript", "require('fs').writeFileSync('output/a.txt', 'x')\nrequire('fs').linkSync('main.js', 'output/copy')", nil, ClassPolicyDenied, false},
		{"python crash", "python", "open('output/a.txt', 'w').write('x')\nimport ctypes\nctypes.string_at(0)", nil, ClassRuntimeCrashed, false},
		{"python file limit", "python", writeBytes("python", "output/a.txt", 1<<20), fileLimit, ClassFileLimit, false},
		{"javascript file limit", "javascript", writeBytes("javascript", "output/a.txt", 1<<20), fileLimit, ClassFileLimit, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored := host.storedRunner(t, store.DefaultArtifactQuotaBytes, tc.change)
			outcome := stored.run(t, tc.runtime, tc.source, "a.txt")
			if outcome.Class != tc.want {
				t.Fatalf("class = %s, want %s: %+v", outcome.Class, tc.want, outcome)
			}
			refs := stored.count(t, "artifact_ref")
			if tc.imported && (refs != 1 || len(outcome.Artifacts) != 1 || outcome.Artifacts[0].Refused != "") {
				t.Fatalf("a %s run must import its output: %+v", tc.want, outcome.Artifacts)
			}
			if !tc.imported && (refs != 0 || outcome.Artifacts != nil) {
				t.Fatalf("a %s run imported %+v", tc.want, outcome.Artifacts)
			}
			host.waitClean(t)
		})
	}
}

func TestLiveFileAtTheSizeLimitIsImported(t *testing.T) {
	const limit = 64 << 10
	for _, runtime := range []string{"python", "javascript"} {
		t.Run(runtime, func(t *testing.T) {
			stored := host.storedRunner(t, store.DefaultArtifactQuotaBytes, func(l *Limits) { l.MaxFileBytes = limit })
			outcome := stored.run(t, runtime, writeBytes(runtime, "output/a.bin", limit), "a.bin")
			if outcome.Class != ClassOK || len(outcome.Artifacts) != 1 || outcome.Artifacts[0].Refused != "" || outcome.Artifacts[0].Size != limit {
				t.Fatalf("outcome = %+v", outcome)
			}
			content, _ := stored.read(t, outcome.Artifacts[0].ArtifactID)
			if len(content) != limit {
				t.Fatalf("stored %d bytes, want %d", len(content), limit)
			}
		})
	}
}

func TestLiveQuotaExhaustionKeepsTheRunClassAndLeavesNoPartialArtifact(t *testing.T) {
	for _, runtime := range []string{"python", "javascript"} {
		t.Run(runtime, func(t *testing.T) {
			stored := host.storedRunner(t, 100, nil)
			source := writeBytes(runtime, "output/small.bin", 50) + writeBytes(runtime, "output/big.bin", 400)
			outcome := stored.run(t, runtime, source, "small.bin", "big.bin")
			if outcome.Class != ClassOK || len(outcome.Artifacts) != 2 || outcome.Artifacts[0].Refused != "" || outcome.Artifacts[1].Refused != RefusalQuota {
				t.Fatalf("outcome = %+v", outcome)
			}
			if blobs, refs := stored.count(t, "blob"), stored.count(t, "artifact_ref"); blobs != 1 || refs != 1 {
				t.Fatalf("blobs = %d, refs = %d, want only the artifact that fit", blobs, refs)
			}
		})
	}
}

func TestLiveSpecialFilesCannotBeMadeByTheProgram(t *testing.T) {
	cases := []struct {
		name    string
		runtime string
		source  string
	}{
		{"python symlink", "python", "import os\nos.symlink('/etc/passwd', 'output/x')"},
		{"python hard link", "python", "import os\nopen('real', 'w').write('x')\nos.link('real', 'output/x')"},
		{"python fifo", "python", "import os\nos.mkfifo('output/x')"},
		{"python device", "python", "import os\nos.mknod('output/x', 0o600 | 0o020000, os.makedev(1, 3))"},
		{"javascript symlink", "javascript", "require('fs').symlinkSync('/etc/passwd', 'output/x')"},
		{"javascript hard link", "javascript", "const fs = require('fs')\nfs.writeFileSync('real', 'x')\nfs.linkSync('real', 'output/x')"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored := host.storedRunner(t, store.DefaultArtifactQuotaBytes, nil)
			outcome := stored.run(t, tc.runtime, tc.source, "x")
			created := outcome.Class == ClassOK
			if created {
				if len(outcome.Artifacts) != 1 || outcome.Artifacts[0].Refused == "" {
					t.Fatalf("the program made a special file and it was imported: %+v", outcome)
				}
			}
			if refs := stored.count(t, "artifact_ref"); refs != 0 {
				t.Fatalf("artifact refs = %d, want none", refs)
			}
			t.Logf("class=%s exit=%d signal=%d artifacts=%+v", outcome.Class, outcome.ExitCode, outcome.Signal, outcome.Artifacts)
		})
	}
}

func TestLiveSweepSparesARunThatIsStillExecuting(t *testing.T) {
	sources := map[string]string{
		"python":     "import os, time\nopen('ready', 'w').close()\nwhile not os.path.exists('go'):\n    time.sleep(0.02)\nopen('output/a.txt', 'w').write('late')\n",
		"javascript": "const fs = require('fs')\nfs.closeSync(fs.openSync('ready', 'w'))\nconst wait = () => fs.existsSync('go') ? fs.writeFileSync('output/a.txt', 'late') : setTimeout(wait, 20)\nwait()\n",
	}
	for runtime, source := range sources {
		t.Run(runtime, func(t *testing.T) {
			stored := host.storedRunner(t, store.DefaultArtifactQuotaBytes, nil)
			done := make(chan Outcome, 1)
			go func() { done <- stored.run(t, runtime, source, "a.txt") }()
			host.waitForFile(t, "ready")
			sweeper := host.runnerWith(t, func(*Limits) {})
			removed, err := sweeper.SweepStale(t.Context())
			if err != nil || removed != 0 {
				t.Fatalf("SweepStale during a live run = %d, %v; want 0, nil", removed, err)
			}
			matches, err := filepath.Glob(filepath.Join(host.data, "runs", runDirPrefix+"*", "ready"))
			if err != nil || len(matches) != 1 {
				t.Fatalf("the live run directory is gone: %v, %v", matches, err)
			}
			if err := touch(filepath.Join(filepath.Dir(matches[0]), "go")); err != nil {
				t.Fatal(err)
			}
			outcome := <-done
			if outcome.Class != ClassOK || len(outcome.Artifacts) != 1 || outcome.Artifacts[0].Refused != "" {
				t.Fatalf("outcome = %+v", outcome)
			}
			host.waitClean(t)
		})
	}
}

func TestLiveNothingCrossesInvocations(t *testing.T) {
	first := map[string]string{
		"python": "import os, sys, subprocess, tempfile\nkeys = sorted(os.environ)\n" +
			"open('leak.txt', 'w').write('x')\nos.makedirs('.cache/deep')\nopen('.cache/deep/c', 'w').write('x')\n" +
			"open(os.path.join(os.environ['HOME'], 'home.txt'), 'w').write('x')\nopen(os.path.join(tempfile.gettempdir(), 'tmp.txt'), 'w').write('x')\n" +
			"open('output/o.txt', 'w').write('x')\nopen('leak_mod.py', 'w').write('STATE = []\\n')\nsys.path.insert(0, os.getcwd())\nimport leak_mod\nleak_mod.STATE.append('dirty')\n" +
			"os.environ['LEAK'] = '1'\nsys.leaked = 'x'\n" +
			"child = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(120)'])\nopen('child_pid', 'w').write(str(child.pid))\n" +
			"print(keys)\nprint(sorted(os.listdir('.')))\n",
		"javascript": "const fs = require('fs'), cp = require('child_process'), path = require('path'), os = require('os')\nconst keys = Object.keys(process.env).sort()\n" +
			"fs.writeFileSync('leak.txt', 'x')\nfs.mkdirSync('.cache/deep', { recursive: true })\nfs.writeFileSync('.cache/deep/c', 'x')\n" +
			"fs.writeFileSync(path.join(process.env.HOME, 'home.txt'), 'x')\nfs.writeFileSync(path.join(os.tmpdir(), 'tmp.txt'), 'x')\n" +
			"fs.writeFileSync('output/o.txt', 'x')\nfs.writeFileSync('leak_mod.js', 'module.exports = { state: [] }')\nrequire('./leak_mod.js').state.push('dirty')\n" +
			"process.env.LEAK = '1'\nglobalThis.leaked = 'x'\n" +
			"const child = cp.spawn(process.execPath, ['-e', 'setTimeout(() => {}, 120000)'], { stdio: 'ignore' })\nfs.writeFileSync('child_pid', String(child.pid))\n" +
			"console.log(JSON.stringify(keys))\nconsole.log(JSON.stringify(fs.readdirSync('.').sort()))\nsetTimeout(() => process.exit(0), 200)\n",
	}
	second := map[string]string{
		"python": "import os, sys\nprint(sorted(os.environ))\nprint(sorted(os.listdir('.')))\nprint(os.environ.get('LEAK'), hasattr(sys, 'leaked'), 'leak_mod' in sys.modules)\n" +
			"sys.path.insert(0, os.getcwd())\ntry:\n    import leak_mod\n    print('imported')\nexcept ImportError:\n    print('absent')\nprint(os.getpid(), os.getppid())\nprint(sorted(os.listdir('output')))\n",
		"javascript": "const fs = require('fs')\nconsole.log(JSON.stringify(Object.keys(process.env).sort()))\nconsole.log(JSON.stringify(fs.readdirSync('.').sort()))\n" +
			"console.log(process.env.LEAK, typeof globalThis.leaked, Object.keys(require.cache).length)\n" +
			"try { require('./leak_mod.js'); console.log('imported') } catch (e) { console.log('absent') }\nconsole.log(process.pid, process.ppid)\nconsole.log(JSON.stringify(fs.readdirSync('output')))\n",
	}
	wantSecond := map[string][]string{
		"python":     {"['main.py', 'output']", "None False False", "absent", "1 0", "[]"},
		"javascript": {`["main.js","output"]`, "undefined undefined 1", "absent", "1 0", "[]"},
	}
	for runtime := range first {
		t.Run(runtime, func(t *testing.T) {
			before := host.treeStatus(t)
			one := host.execute(t, host.runner, runtime, first[runtime], "")
			if one.Class != ClassOK {
				t.Fatalf("first run = %+v", one)
			}
			host.waitClean(t)
			two := host.execute(t, host.runner, runtime, second[runtime], "")
			if two.Class != ClassOK {
				t.Fatalf("second run = %+v", two)
			}
			lines := strings.Split(strings.TrimSpace(two.Stdout), "\n")
			firstLines := strings.Split(strings.TrimSpace(one.Stdout), "\n")
			if len(lines) != len(wantSecond[runtime])+1 || len(firstLines) != 2 {
				t.Fatalf("first stdout = %q, second stdout = %q", one.Stdout, two.Stdout)
			}
			if lines[0] != firstLines[0] {
				t.Fatalf("environment names differ between runs: %s vs %s", firstLines[0], lines[0])
			}
			for i, want := range wantSecond[runtime] {
				if got := lines[i+1]; got != want {
					t.Fatalf("second run line %d = %q, want %q (stdout %q)", i+1, got, want, two.Stdout)
				}
			}
			host.waitClean(t)
			if after := host.treeStatus(t); after != before {
				t.Fatalf("the installed runtimes changed: %v -> %v", before, after)
			}
		})
	}
}

func touch(path string) error {
	return os.WriteFile(path, nil, 0o600)
}

func (h *liveHost) treeStatus(t *testing.T) string {
	t.Helper()
	set, err := interpreter.NewVerifiedSet(h.store.Manifest())
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := interpreter.NewVerifier(h.store, set, interpreter.WithProber(h.runner))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	var states []string
	for _, status := range set.Statuses() {
		states = append(states, status.Runtime+"="+string(status.State)+"/"+string(status.Reason))
	}
	return strings.Join(states, ",")
}
