//go:build linux && integration

package codeexec

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/tools/interpreter"
)

const (
	cleanupDeadline = 10 * time.Second
	pollInterval    = 25 * time.Millisecond
)

func (h *liveHost) waitClean(t *testing.T) {
	t.Helper()
	deadline := time.NewTimer(cleanupDeadline)
	defer deadline.Stop()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		left := h.leftovers(t)
		if len(left) == 0 {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("left behind after the run: %v", left)
		case <-ticker.C:
		}
	}
}

func (h *liveHost) waitForFile(t *testing.T, name string) {
	t.Helper()
	deadline := time.NewTimer(cleanupDeadline)
	defer deadline.Stop()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		if matches, _ := filepath.Glob(filepath.Join(h.data, "runs", runDirPrefix+"*", name)); len(matches) > 0 {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("the program never created %s", name)
		case <-ticker.C:
		}
	}
}

type liveRun struct {
	outcome Outcome
	err     error
}

func TestLiveResultClasses(t *testing.T) {
	short := func(l *Limits) { l.Timeout = 2 * time.Second }
	cases := []struct {
		name    string
		runtime string
		source  string
		change  func(*Limits)
		want    ResultClass
		exit    int
		signal  syscall.Signal
	}{
		{name: "python ok", runtime: "python", source: "print(1)", want: ClassOK},
		{name: "javascript ok", runtime: "javascript", source: "console.log(1)", want: ClassOK},
		{name: "python exit code", runtime: "python", source: "import sys\nsys.exit(3)", want: ClassExitNonzero, exit: 3},
		{name: "javascript exit code", runtime: "javascript", source: "process.exit(3)", want: ClassExitNonzero, exit: 3},
		{name: "python exception", runtime: "python", source: "raise ValueError('x')", want: ClassExitNonzero, exit: 1},
		{name: "javascript exception", runtime: "javascript", source: "throw new Error('x')", want: ClassExitNonzero, exit: 1},
		{name: "python timeout", runtime: "python", source: "import time\ntime.sleep(60)", change: short, want: ClassTimeout},
		{name: "javascript timeout", runtime: "javascript", source: "setTimeout(() => {}, 60000)", change: short, want: ClassTimeout},
		{name: "python cpu limit", runtime: "python", source: "while True:\n    pass", change: short, want: ClassCPULimit, signal: syscall.SIGKILL},
		{name: "javascript cpu limit", runtime: "javascript", source: "while (true) {}", change: short, want: ClassCPULimit, signal: syscall.SIGKILL},
		{
			name: "python memory limit", runtime: "python", source: "print('before', flush=True)\nblock = b'x' * (1 << 30)\nprint(len(block))",
			change: func(l *Limits) { l.MaxMemoryBytes = 256 << 20 }, want: ClassMemoryLimit, signal: syscall.SIGKILL,
		},
		{
			name: "javascript memory limit", runtime: "javascript", source: "console.log('before')\nconst block = Buffer.alloc(1 << 30, 1)\nconsole.log(block.length)",
			change: func(l *Limits) { l.MaxMemoryBytes = 256 << 20 }, want: ClassMemoryLimit, signal: syscall.SIGKILL,
		},
		{
			name: "python process limit", runtime: "python", source: "import threading\nstop = threading.Event()\nfor _ in range(200):\n    threading.Thread(target=stop.wait, daemon=True).start()",
			change: func(l *Limits) { l.MaxProcesses = 16 }, want: ClassProcessLimit, exit: 1,
		},
		{
			name: "javascript process limit", runtime: "javascript", source: "const { Worker } = require('worker_threads')\nprocess.on('uncaughtException', () => process.exit(7))\nfor (let i = 0; i < 200; i++) new Worker('setTimeout(() => {}, 60000)', { eval: true }).on('error', () => process.exit(7))",
			change: func(l *Limits) { l.MaxProcesses = 20 }, want: ClassProcessLimit, exit: 7,
		},
		{
			name: "python file limit", runtime: "python", source: "open('big', 'wb').write(b'x' * (4 << 20))",
			change: func(l *Limits) { l.MaxFileBytes = 1 << 20 }, want: ClassFileLimit, exit: 1,
		},
		{
			name: "javascript file limit", runtime: "javascript", source: "require('fs').writeFileSync('big', Buffer.alloc(4 << 20, 1))",
			change: func(l *Limits) { l.MaxFileBytes = 1 << 20 }, want: ClassFileLimit, exit: 1,
		},
		{
			name: "python disk limit", runtime: "python", source: "import time\nfor i in range(400):\n    open('f%d' % i, 'wb').write(b'x' * (1 << 20))\n    time.sleep(0.05)",
			change: func(l *Limits) { l.MaxRunDirBytes = 6 << 20; l.MaxFileBytes = 2 << 20 }, want: ClassDiskLimit, signal: syscall.SIGKILL,
		},
		{
			name: "javascript disk limit", runtime: "javascript", source: "const fs = require('fs')\nlet i = 0\nsetInterval(() => fs.writeFileSync('f' + i++, Buffer.alloc(1 << 20, 1)), 50)",
			change: func(l *Limits) { l.MaxRunDirBytes = 6 << 20; l.MaxFileBytes = 2 << 20 }, want: ClassDiskLimit, signal: syscall.SIGKILL,
		},
		{name: "python seccomp kill", runtime: "python", source: "import os\nos.kill(os.getpid(), 0)", want: ClassPolicyDenied, signal: syscall.SIGSYS},
		{name: "javascript seccomp kill", runtime: "javascript", source: "process.kill(process.pid, 0)", want: ClassPolicyDenied, signal: syscall.SIGSYS},
		{name: "python symlink seccomp kill", runtime: "python", source: "import os\nos.symlink('/etc/passwd', 'link')", want: ClassPolicyDenied, signal: syscall.SIGSYS},
		{name: "javascript hard link seccomp kill", runtime: "javascript", source: "require('fs').linkSync('main.js', 'copy')", want: ClassPolicyDenied, signal: syscall.SIGSYS},
		{name: "python crash", runtime: "python", source: "import ctypes\nctypes.string_at(0)", want: ClassRuntimeCrashed, signal: syscall.SIGSEGV},
		{name: "javascript abort", runtime: "javascript", source: "process.abort()", want: ClassRuntimeCrashed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := host.runner
			if tc.change != nil {
				runner = host.runnerWith(t, tc.change)
			}
			outcome := host.execute(t, runner, tc.runtime, tc.source, "")
			if outcome.Class != tc.want {
				t.Fatalf("class = %s, want %s: %+v", outcome.Class, tc.want, outcome)
			}
			if tc.exit != 0 && outcome.ExitCode != tc.exit {
				t.Fatalf("exit code = %d, want %d: %+v", outcome.ExitCode, tc.exit, outcome)
			}
			if tc.signal != 0 && outcome.Signal != int(tc.signal) {
				t.Fatalf("signal = %d, want %d: %+v", outcome.Signal, tc.signal, outcome)
			}
			host.waitClean(t)
		})
	}
}

func TestLiveCancelIsDistinctFromTimeout(t *testing.T) {
	sources := map[string]string{
		"python":     "open('ready', 'w').close()\nimport time\ntime.sleep(60)",
		"javascript": "require('fs').closeSync(require('fs').openSync('ready', 'w'))\nsetTimeout(() => {}, 60000)",
	}
	for runtime, source := range sources {
		t.Run(runtime, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan liveRun, 1)
			go func() {
				outcome, err := host.runner.Execute(ctx, &Request{Runtime: runtime, Source: source})
				done <- liveRun{outcome, err}
			}()
			host.waitForFile(t, "ready")
			cancel()
			run := <-done
			if run.err != nil {
				t.Fatalf("Execute: %v", run.err)
			}
			if run.outcome.Class != ClassCancelled || run.outcome.Signal != int(syscall.SIGKILL) {
				t.Fatalf("outcome = %+v", run.outcome)
			}
			host.waitClean(t)
		})
	}
}

func TestLiveDescendantsAreReaped(t *testing.T) {
	sources := map[string]string{
		"python":     "import os, time\nif os.fork() == 0:\n    time.sleep(120)\nelse:\n    print('parent done')",
		"javascript": "const { Worker } = require('worker_threads')\nnew Worker('setTimeout(() => {}, 120000)', { eval: true })\nsetTimeout(() => process.exit(0), 200)",
	}
	for runtime, source := range sources {
		t.Run(runtime, func(t *testing.T) {
			outcome := host.execute(t, host.runner, runtime, source, "")
			if outcome.Class != ClassOK {
				t.Fatalf("outcome = %+v", outcome)
			}
			host.waitClean(t)
		})
	}
}

func TestLiveRunsDoNotShareState(t *testing.T) {
	first := map[string]string{
		"python":     "import os\nopen('leak', 'w').write('x')\nos.environ['LEAK'] = '1'\nprint(sorted(os.listdir('.')))",
		"javascript": "require('fs').writeFileSync('leak', 'x')\nconsole.log(require('fs').readdirSync('.').sort())",
	}
	second := map[string]string{
		"python":     "import os\nprint(sorted(os.listdir('.')), os.environ.get('LEAK'))",
		"javascript": "console.log(require('fs').readdirSync('.').sort(), process.env.LEAK)",
	}
	wantSecond := map[string]string{"python": "['main.py', 'output'] None", "javascript": "[ 'main.js', 'output' ] undefined"}
	for runtime := range first {
		t.Run(runtime, func(t *testing.T) {
			host.execute(t, host.runner, runtime, first[runtime], "")
			outcome := host.execute(t, host.runner, runtime, second[runtime], "")
			if outcome.Class != ClassOK || strings.TrimSpace(outcome.Stdout) != wantSecond[runtime] {
				t.Fatalf("outcome = %+v", outcome)
			}
		})
	}
}

func TestLiveTamperedBundleIsUnavailable(t *testing.T) {
	bundle, err := host.store.Bundle("python")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(bundle.Dir, "python", "lib", "python3.12", "os.py")
	original, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(target, 0o644)
		_ = os.WriteFile(target, original, 0o644)
		_ = os.Chmod(target, info.Mode())
	})
	if err := os.WriteFile(target, append(append([]byte(nil), original...), '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	set, err := interpreter.NewVerifiedSet(host.store.Manifest())
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(&Options{Store: host.store, Verified: set, Artifacts: &memorySink{}, RunRoot: filepath.Join(host.data, "runs"), Limits: liveLimits()})
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := interpreter.NewVerifier(host.store, set, interpreter.WithProber(runner))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, err = runner.Execute(t.Context(), &Request{Runtime: "python", Source: "print(1)"})
	if code, _ := CodeOf(err); code != ErrorCodeUnavailable {
		t.Fatalf("Execute on a tampered bundle = %v, want %s", err, ErrorCodeUnavailable)
	}
	if outcome := host.execute(t, runner, "javascript", "console.log(1)", ""); outcome.Class != ClassOK {
		t.Fatalf("the untouched runtime must stay available: %+v", outcome)
	}
	host.waitClean(t)
}

func TestLiveGoIsUnavailable(t *testing.T) {
	_, err := host.runner.Execute(t.Context(), &Request{Runtime: "go", Source: "package main\nfunc main() {}"})
	if code, _ := CodeOf(err); code != ErrorCodeUnavailable {
		t.Fatalf("Execute(go) = %v, want %s", err, ErrorCodeUnavailable)
	}
	host.waitClean(t)
}

func TestLiveSyscallsTheRuntimesNeed(t *testing.T) {
	cases := []struct {
		syscall string
		runtime string
		source  string
	}{
		{"capget", "javascript", "console.log('ok')"},
		{"eventfd2", "javascript", "console.log('ok')"},
		{"epoll_pwait", "javascript", "setTimeout(() => console.log('ok'), 10)"},
		{"sched_getscheduler", "javascript", "const a = []\nfor (let i = 0; i < 3e5; i++) a.push({ i, s: 'x' + i })\nconsole.log('ok', JSON.stringify(a).length > 0)"},
		{"sched_getparam", "javascript", "const a = []\nfor (let i = 0; i < 3e5; i++) a.push({ i, s: 'x' + i })\nconsole.log('ok', JSON.stringify(a).length > 0)"},
		{"mkdir", "python", "import os\nos.mkdir('d')\nprint('ok')"},
		{"mkdir", "javascript", "require('fs').mkdirSync('d')\nconsole.log('ok')"},
		{"rmdir", "python", "import os\nos.mkdir('d')\nos.rmdir('d')\nprint('ok')"},
		{"rmdir", "javascript", "const fs = require('fs')\nfs.mkdirSync('d')\nfs.rmdirSync('d')\nconsole.log('ok')"},
		{"unlink", "python", "import os\nopen('f', 'w').close()\nos.unlink('f')\nprint('ok')"},
		{"unlink", "javascript", "const fs = require('fs')\nfs.writeFileSync('f', '')\nfs.unlinkSync('f')\nconsole.log('ok')"},
		{"rename", "python", "import os\nopen('f', 'w').close()\nos.rename('f', 'g')\nprint('ok')"},
		{"rename", "javascript", "const fs = require('fs')\nfs.writeFileSync('f', '')\nfs.renameSync('f', 'g')\nconsole.log('ok')"},
		{"renameat", "python", "import os\nopen('f', 'w').close()\nfd = os.open('.', os.O_RDONLY)\nos.rename('f', 'g', src_dir_fd=fd, dst_dir_fd=fd)\nprint('ok')"},
		{"renameat2", "python", "import ctypes\nopen('f', 'w').close()\nassert ctypes.CDLL(None).renameat2(-100, b'f', -100, b'g', 1) == 0\nprint('ok')"},
		{"unlinkat", "python", "import os, shutil\nos.mkdir('d')\nopen('d/f', 'w').close()\nshutil.rmtree('d')\nprint('ok')"},
		{"unlinkat", "javascript", "const fs = require('fs')\nfs.mkdirSync('d')\nfs.writeFileSync('d/f', '')\nfs.rmSync('d', { recursive: true })\nconsole.log('ok')"},
		{"fsync", "python", "import os\nfd = os.open('f', os.O_CREAT | os.O_WRONLY)\nos.fsync(fd)\nprint('ok')"},
		{"fsync", "javascript", "const fs = require('fs')\nfs.fsyncSync(fs.openSync('f', 'w'))\nconsole.log('ok')"},
		{"sendfile", "python", "import shutil\nopen('a', 'w').write('hello')\nshutil.copyfile('a', 'b')\nprint('ok')"},
		{"sendto", "python", "import socket\na, b = socket.socketpair()\na.send(b'x')\nassert b.recv(1) == b'x'\na.close()\nb.close()\nprint('ok')"},
		{"recvfrom", "python", "import socket\na, b = socket.socketpair()\na.send(b'x')\nassert b.recv(1) == b'x'\na.close()\nb.close()\nprint('ok')"},
		{"getsockname", "python", "import socket\na, b = socket.socketpair()\na.getsockname()\na.close()\nb.close()\nprint('ok')"},
		{"getsockopt", "python", "import socket\na, b = socket.socketpair()\na.getsockopt(socket.SOL_SOCKET, socket.SO_TYPE)\na.close()\nb.close()\nprint('ok')"},
		{"getpeername", "python", "import socket\na, b = socket.socketpair()\nprint('ok')"},
		{"tgkill", "python", "import signal, threading\nsignal.signal(signal.SIGUSR1, lambda *args: None)\nsignal.pthread_kill(threading.get_ident(), signal.SIGUSR1)\nprint('ok')"},
	}
	for _, tc := range cases {
		t.Run(tc.syscall+"/"+tc.runtime, func(t *testing.T) {
			outcome := host.execute(t, host.runner, tc.runtime, tc.source, "")
			if outcome.Class != ClassOK || !strings.Contains(outcome.Stdout, "ok") {
				t.Fatalf("outcome = %+v", outcome)
			}
		})
	}
}

func TestLiveHostFilesCannotBeChangedOrRemoved(t *testing.T) {
	dir := t.TempDir()
	canary := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(canary, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	sources := map[string]string{
		"python": fmt.Sprintf(`import os
d, t = %q, %q
steps = {
    'write': lambda: open(t, 'w').write('x'),
    'append': lambda: open(t, 'a').write('x'),
    'remove': lambda: os.remove(t),
    'rename': lambda: os.rename(t, t + '.moved'),
    'create': lambda: open(d + '/new.txt', 'w').close(),
    'mkdir': lambda: os.mkdir(d + '/sub'),
    'truncate': lambda: os.truncate(t, 0),
}
for name, step in steps.items():
    try:
        step(); print(name, 'changed')
    except OSError:
        print(name, 'denied')
`, dir, canary),
		"javascript": fmt.Sprintf(`const fs = require('fs')
const d = %q, t = %q
const steps = {
  write: () => fs.writeFileSync(t, 'x'),
  append: () => fs.appendFileSync(t, 'x'),
  remove: () => fs.unlinkSync(t),
  rename: () => fs.renameSync(t, t + '.moved'),
  create: () => fs.closeSync(fs.openSync(d + '/new.txt', 'w')),
  mkdir: () => fs.mkdirSync(d + '/sub'),
  truncate: () => fs.truncateSync(t, 0),
}
for (const [name, step] of Object.entries(steps)) {
  try { step(); console.log(name, 'changed') } catch (e) { console.log(name, 'denied') }
}
`, dir, canary),
	}
	for runtime, source := range sources {
		t.Run(runtime, func(t *testing.T) {
			outcome := host.execute(t, host.runner, runtime, source, "")
			if outcome.Class != ClassOK || strings.Contains(outcome.Stdout, "changed") || strings.Count(outcome.Stdout, "denied") != 7 {
				t.Fatalf("outcome = %+v", outcome)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != "victim.txt" {
				t.Fatalf("the host directory changed: %v %v", entries, err)
			}
			if data, err := os.ReadFile(canary); err != nil || string(data) != "data" {
				t.Fatalf("the canary changed: %q %v", data, err)
			}
		})
	}
}

func TestLiveRunDirectoryWithLockedEntriesIsRemoved(t *testing.T) {
	sources := map[string]string{
		"python":     "import os\nos.mkdir('locked', 0)\nos.mkdir('open')\nprint('ok')",
		"javascript": "const fs = require('fs')\nfs.mkdirSync('locked', { mode: 0 })\nfs.mkdirSync('open')\nconsole.log('ok')",
	}
	for runtime, source := range sources {
		t.Run(runtime, func(t *testing.T) {
			outcome := host.execute(t, host.runner, runtime, source, "")
			if outcome.Class != ClassOK {
				t.Fatalf("outcome = %+v", outcome)
			}
			host.waitClean(t)
		})
	}
}
