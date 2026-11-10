//go:build linux && integration

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const (
	startLoopCount    = 40
	startLoopParallel = 8
	probeTimeout      = 30 * time.Second
)

type probeReply struct {
	Mode  string `json:"mode"`
	Errno int    `json:"errno"`
	Note  string `json:"note"`
}

var probeBinaryPath string

func buildProbeBinary() (dir string, err error) {
	dir, err = os.MkdirTemp("", "aura-probe-build-")
	if err != nil {
		return "", err
	}
	goBinary, err := exec.LookPath("go")
	if err != nil {
		return dir, err
	}
	probeBinaryPath = filepath.Join(dir, "probe")
	build := exec.CommandContext(context.Background(), goBinary, "build", "-o", probeBinaryPath, "github.com/anggasct/aura/internal/platform/sandbox/testdata/probe")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		return dir, fmt.Errorf("%w: %s", err, output)
	}
	return dir, nil
}

func probeBinary(t *testing.T) string {
	t.Helper()
	if probeBinaryPath == "" {
		t.Fatal("the probe connector was not built")
	}
	return probeBinaryPath
}

func connectorBundle(t *testing.T) (dir, executable string) {
	t.Helper()
	dir = t.TempDir()
	executable = filepath.Join(dir, "probe")
	binary, err := os.ReadFile(probeBinary(t))
	if err != nil {
		t.Fatalf("read the probe connector: %v", err)
	}
	if err := os.WriteFile(executable, binary, 0o700); err != nil {
		t.Fatalf("write the probe connector: %v", err)
	}
	return dir, executable
}

func connectorSessionRequest(t *testing.T) *SessionRequest {
	t.Helper()
	dir, executable := connectorBundle(t)
	return &SessionRequest{
		Profile:      ProfileConnector,
		RequestID:    "connector-request",
		SessionID:    "connector-session",
		Executable:   executable,
		WorkingDir:   dir,
		Environment:  map[string]string{},
		Limits:       Limits{Timeout: probeTimeout, MemoryBytes: 512 << 20, MaxProcesses: 256, MaxOpenFiles: 256},
		SetupTimeout: probeTimeout,
	}
}

func startConnector(t *testing.T, req *SessionRequest) *Session {
	t.Helper()
	session, err := Start(t.Context(), req)
	if err != nil {
		t.Fatalf("Start connector session: %v", err)
	}
	t.Cleanup(func() { _ = session.Close(context.WithoutCancel(t.Context())) })
	return session
}

func probe(t *testing.T, session *Session, command string) (probeReply, error) {
	t.Helper()
	raw, err := session.Request(t.Context(), []byte(command))
	if err != nil {
		return probeReply{}, err
	}
	var reply probeReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatalf("decode probe reply %q: %v", raw, err)
	}
	return reply, nil
}

func startLoop(t *testing.T, total, workers int, once func(index int) error) {
	t.Helper()
	var next atomic.Int64
	var failures atomic.Int64
	var mu sync.Mutex
	var firstErrors []string
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				index := int(next.Add(1)) - 1
				if index >= total {
					return
				}
				if err := once(index); err != nil {
					failures.Add(1)
					mu.Lock()
					if len(firstErrors) < 5 {
						firstErrors = append(firstErrors, fmt.Sprintf("start %d: %v", index, err))
					}
					mu.Unlock()
				}
			}
		}()
	}
	group.Wait()
	t.Logf("starts=%d workers=%d failed=%d", total, workers, failures.Load())
	if failures.Load() > 0 {
		t.Fatalf("%d of %d starts failed: %s", failures.Load(), total, strings.Join(firstErrors, "; "))
	}
}

func connectorStartOnce(ctx context.Context, req *SessionRequest) error {
	session, err := Start(ctx, req)
	if err != nil {
		return err
	}
	defer func() { _ = session.Close(context.WithoutCancel(ctx)) }()
	raw, err := session.Request(ctx, []byte("work"))
	if err != nil {
		return err
	}
	if !strings.Contains(string(raw), `"mode":"work"`) {
		return fmt.Errorf("unexpected reply %q", raw)
	}
	return nil
}

func TestIntegrationStartLoopConnectorSerial(t *testing.T) {
	req := connectorSessionRequest(t)
	startLoop(t, startLoopCount, 1, func(int) error { return connectorStartOnce(t.Context(), req) })
}

func TestIntegrationStartLoopConnectorParallel(t *testing.T) {
	req := connectorSessionRequest(t)
	startLoop(t, startLoopCount, startLoopParallel, func(int) error { return connectorStartOnce(t.Context(), req) })
}

func runBaseOnce(ctx context.Context, spec *Spec) error {
	result, err := Run(ctx, spec, "/bin/sh", "-c", "echo started")
	if err != nil {
		return err
	}
	if result.ExitCode != 0 || result.Output != "started" {
		return fmt.Errorf("exit=%d output=%q", result.ExitCode, result.Output)
	}
	return nil
}

func TestIntegrationStartLoopBaseSerial(t *testing.T) {
	spec := baseSpec(t)
	startLoop(t, startLoopCount, 1, func(int) error { return runBaseOnce(t.Context(), &spec) })
}

func TestIntegrationStartLoopBaseParallel(t *testing.T) {
	spec := baseSpec(t)
	startLoop(t, startLoopCount, startLoopParallel, func(int) error { return runBaseOnce(t.Context(), &spec) })
}

func linuxSessionOf(t *testing.T, session *Session) *linuxSession {
	t.Helper()
	linux, ok := session.sessionAPI.(*linuxSession)
	if !ok {
		t.Fatalf("session implementation = %T", session.sessionAPI)
	}
	return linux
}

func awaitChildExit(t *testing.T, session *Session) error {
	t.Helper()
	linux := linuxSessionOf(t, session)
	select {
	case <-linux.reapDone:
	case <-time.After(probeTimeout):
		t.Fatal("the session child did not exit")
	}
	return linux.reapErr
}

func awaitChildSignal(t *testing.T, session *Session) syscall.Signal {
	t.Helper()
	exit := awaitChildExit(t, session)
	var exitErr *exec.ExitError
	if !errors.As(exit, &exitErr) {
		t.Fatalf("child exit = %v, want a signal exit", exit)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		t.Fatalf("child exit = %v, want a signal exit", exitErr)
	}
	return status.Signal()
}

type victim struct {
	pid    int
	exited chan struct{}
}

func startVictim(t *testing.T) *victim {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sleep", "3600")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the host victim: %v", err)
	}
	v := &victim{pid: cmd.Process.Pid, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(v.exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-v.exited
	})
	return v
}

func (v *victim) openFilesLimit(t *testing.T) string {
	t.Helper()
	limits, err := os.ReadFile(fmt.Sprintf("/proc/%d/limits", v.pid))
	if err != nil {
		t.Fatalf("read victim limits: %v", err)
	}
	for line := range strings.SplitSeq(string(limits), "\n") {
		if strings.HasPrefix(line, "Max open files") {
			return strings.Join(strings.Fields(line), " ")
		}
	}
	t.Fatalf("victim limits have no open files row: %s", limits)
	return ""
}

func (v *victim) requireUntouched(t *testing.T, limit string) {
	t.Helper()
	select {
	case <-v.exited:
		t.Fatal("the host victim process was killed from inside the sandbox")
	case <-time.After(time.Second):
	}
	if got := v.openFilesLimit(t); got != limit {
		t.Fatalf("victim open files limit = %q, want %q", got, limit)
	}
}

var hostAttacks = []string{"kill", "tgkill", "setown", "prlimit"}

func TestIntegrationAttackControlReachesAHostProcessWithoutTheSandbox(t *testing.T) {
	for _, mode := range hostAttacks {
		t.Run(mode, func(t *testing.T) {
			target := startVictim(t)
			before := target.openFilesLimit(t)
			output, err := exec.CommandContext(t.Context(), probeBinary(t), mode, strconv.Itoa(target.pid)).CombinedOutput()
			if err != nil {
				t.Logf("attacker exit: %v %s", err, output)
			}
			if mode == "prlimit" {
				if got := target.openFilesLimit(t); got == before {
					t.Fatalf("control attack left the limit at %q", got)
				}
				return
			}
			select {
			case <-target.exited:
			case <-time.After(probeTimeout):
				t.Fatalf("control attack %q did not kill the victim: %s", mode, output)
			}
		})
	}
}

func TestIntegrationSessionCannotReachAHostProcess(t *testing.T) {
	for _, mode := range hostAttacks {
		t.Run(mode, func(t *testing.T) {
			target := startVictim(t)
			before := target.openFilesLimit(t)
			session := startConnector(t, connectorSessionRequest(t))
			if _, err := probe(t, session, mode+" "+strconv.Itoa(target.pid)); err == nil {
				t.Fatal("the attack returned instead of being stopped")
			}
			if got := awaitChildSignal(t, session); got != syscall.SIGSYS {
				t.Fatalf("child signal = %v, want SIGSYS", got)
			}
			target.requireUntouched(t, before)
		})
	}
}

func TestIntegrationRunCannotReachAHostProcess(t *testing.T) {
	for _, mode := range hostAttacks {
		t.Run(mode, func(t *testing.T) {
			target := startVictim(t)
			before := target.openFilesLimit(t)
			dir, executable := connectorBundle(t)
			spec := Spec{
				Profile: ProfileConnector, WorkingDir: dir,
				AllowEnv: []string{}, Limits: Limits{Timeout: probeTimeout},
			}
			result, err := Run(t.Context(), &spec, executable, mode, strconv.Itoa(target.pid))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if result.ExitCode == 0 {
				t.Fatalf("the attack returned instead of being stopped: %q", result.Output)
			}
			target.requireUntouched(t, before)
		})
	}
}

func TestIntegrationBaseRunCannotSignalAHostProcess(t *testing.T) {
	target := startVictim(t)
	before := target.openFilesLimit(t)
	spec := baseSpec(t)
	result, err := Run(t.Context(), &spec, "/bin/sh", "-c", fmt.Sprintf("kill -9 %d; echo survived", target.pid))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode == 0 || strings.Contains(result.Output, "survived") {
		t.Fatalf("kill returned instead of being stopped: %+v", result)
	}
	target.requireUntouched(t, before)
}

func TestIntegrationBaseSessionCannotSignalAHostProcess(t *testing.T) {
	target := startVictim(t)
	before := target.openFilesLimit(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "killer.sh")
	body := "#!/bin/sh\nwhile IFS= read -r pid; do\n  kill -9 \"$pid\"\n  echo survived\ndone\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write the session script: %v", err)
	}
	req := &SessionRequest{
		RequestID: "base-session", SessionID: "base-session", Executable: script, WorkingDir: dir,
		Environment: map[string]string{}, Limits: Limits{Timeout: probeTimeout}, SetupTimeout: probeTimeout,
	}
	session := startConnector(t, req)
	if reply, err := session.Request(t.Context(), []byte(strconv.Itoa(target.pid))); err == nil {
		t.Fatalf("kill returned instead of being stopped: %q", reply)
	}
	if got := awaitChildSignal(t, session); got != syscall.SIGSYS {
		t.Fatalf("child signal = %v, want SIGSYS", got)
	}
	target.requireUntouched(t, before)
}

func TestIntegrationTargetRunsAsPid1InItsOwnNamespace(t *testing.T) {
	spec := baseSpec(t)
	result, err := Run(t.Context(), &spec, "/bin/sh", "-c", "echo $$")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output != "1" {
		t.Fatalf("pid inside the sandbox = %q, want 1", result.Output)
	}
	session := startConnector(t, connectorSessionRequest(t))
	reply, err := probe(t, session, "pid")
	if err != nil {
		t.Fatalf("pid probe: %v", err)
	}
	if reply.Errno != 1 {
		t.Fatalf("connector pid = %d, want 1", reply.Errno)
	}
}

func TestIntegrationNamespaceCreationIsStopped(t *testing.T) {
	session := startConnector(t, connectorSessionRequest(t))
	if _, err := probe(t, session, "clone-newuser"); err == nil {
		t.Fatal("clone with CLONE_NEWUSER returned instead of being stopped")
	}
	if got := awaitChildSignal(t, session); got != syscall.SIGSYS {
		t.Fatalf("child signal = %v, want SIGSYS", got)
	}
}

func TestIntegrationClone3FallsBackWithENOSYS(t *testing.T) {
	session := startConnector(t, connectorSessionRequest(t))
	reply, err := probe(t, session, "clone3")
	if err != nil {
		t.Fatalf("clone3 probe: %v", err)
	}
	if syscall.Errno(reply.Errno) != syscall.ENOSYS {
		t.Fatalf("clone3 errno = %v, want ENOSYS", syscall.Errno(reply.Errno))
	}
}

func TestIntegrationConnectorProfileStopsEscapeSyscalls(t *testing.T) {
	for _, mode := range []string{"socket", "kill 4242", "ptrace 4242", "bpf", "fork", "tgkill 4242"} {
		t.Run(strings.Fields(mode)[0], func(t *testing.T) {
			session := startConnector(t, connectorSessionRequest(t))
			if _, err := probe(t, session, mode); err == nil {
				t.Fatal("the syscall returned instead of being stopped")
			}
			if got := awaitChildSignal(t, session); got != syscall.SIGSYS {
				t.Fatalf("child signal = %v, want SIGSYS", got)
			}
		})
	}
}

func TestIntegrationConnectorProfileAnswersPrctlWithEPERMAndKeepsRuntimeSignals(t *testing.T) {
	session := startConnector(t, connectorSessionRequest(t))
	reply, err := probe(t, session, "prctl")
	if err != nil {
		t.Fatalf("prctl probe: %v", err)
	}
	if syscall.Errno(reply.Errno) != syscall.EPERM {
		t.Fatalf("prctl errno = %v, want EPERM", syscall.Errno(reply.Errno))
	}
	reply, err = probe(t, session, "tgkill-self")
	if err != nil {
		t.Fatalf("tgkill on the own process: %v", err)
	}
	if reply.Errno != 0 {
		t.Fatalf("tgkill on the own process errno = %v, want success", syscall.Errno(reply.Errno))
	}
}

func TestIntegrationConnectorBundleIsReadOnlyAndScratchIsWritable(t *testing.T) {
	req := connectorSessionRequest(t)
	session := startConnector(t, req)
	reply, err := probe(t, session, "write "+req.WorkingDir)
	if err != nil {
		t.Fatalf("bundle write probe: %v", err)
	}
	if reply.Errno != 0 || !strings.Contains(reply.Note, "permission denied") {
		t.Fatalf("bundle write = %+v, want permission denied", reply)
	}
	if _, err := os.Stat(filepath.Join(req.WorkingDir, "probe-written")); !os.IsNotExist(err) {
		t.Fatalf("the connector wrote into its bundle: %v", err)
	}
	tmp, err := probe(t, session, "tmpdir")
	if err != nil {
		t.Fatalf("tmpdir probe: %v", err)
	}
	if !strings.HasPrefix(filepath.Base(tmp.Note), scratchPattern) {
		t.Fatalf("scratch directory = %q", tmp.Note)
	}
	written, err := probe(t, session, "scratch-write")
	if err != nil || written.Errno != 1 {
		t.Fatalf("scratch write = %+v, %v", written, err)
	}
	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(tmp.Note); !os.IsNotExist(err) {
		t.Fatalf("scratch directory outlived the session: %v", err)
	}
}

func cgroupFile(t *testing.T, session *Session, name string) string {
	t.Helper()
	linux := linuxSessionOf(t, session)
	data, err := os.ReadFile(filepath.Join(cgroupV2Root, linux.cg.name, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return strings.TrimSpace(string(data))
}

func TestIntegrationConnectorPidsLimitApplies(t *testing.T) {
	req := connectorSessionRequest(t)
	req.Limits.MaxProcesses = 64
	session := startConnector(t, req)
	if got := cgroupFile(t, session, "pids.max"); got != "64" {
		t.Fatalf("pids.max = %q, want 64", got)
	}
	if _, err := probe(t, session, "threads 512"); err == nil {
		t.Fatal("the connector created far more threads than its limit")
	}
	if exit := awaitChildExit(t, session); exit == nil {
		t.Fatal("the connector exited cleanly after exhausting its process limit")
	}
}

func TestIntegrationConnectorMemoryLimitApplies(t *testing.T) {
	req := connectorSessionRequest(t)
	req.Limits.MemoryBytes = 96 << 20
	session := startConnector(t, req)
	if got := cgroupFile(t, session, "memory.max"); got != strconv.Itoa(96<<20) {
		t.Fatalf("memory.max = %q, want %d", got, 96<<20)
	}
	if _, err := probe(t, session, "memory 400"); err == nil {
		t.Fatal("the connector allocated far more than its limit")
	}
	awaitChildSignal(t, session)
	events := cgroupFile(t, session, "memory.events")
	if strings.Contains(events, "oom_kill 0") {
		t.Fatalf("memory.events = %q, want an oom kill", events)
	}
}

func TestIntegrationConnectorStderrFloodDoesNotBlock(t *testing.T) {
	session := startConnector(t, connectorSessionRequest(t))
	if _, err := probe(t, session, "stderr 4194304"); err != nil {
		t.Fatalf("stderr flood: %v", err)
	}
	reply, err := probe(t, session, "echo alive")
	if err != nil || reply.Note != "alive" {
		t.Fatalf("exchange after the flood = %+v, %v", reply, err)
	}
}

func TestIntegrationTeardownKillsATargetThatIgnoresSIGTERM(t *testing.T) {
	fdsBefore := countOpenFDs(t)
	cgroupsBefore := countAuraCgroups(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "stubborn.sh")
	body := "#!/bin/sh\ntrap '' TERM\nread -r line\necho ready\nsleep 6047 &\nwhile :; do\n  sleep 6047\ndone\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write the session script: %v", err)
	}
	req := &SessionRequest{
		RequestID: "stubborn", SessionID: "stubborn", Executable: script, WorkingDir: dir,
		Environment: map[string]string{"PATH": os.Getenv("PATH")}, Limits: Limits{Timeout: probeTimeout}, SetupTimeout: probeTimeout,
	}
	session, err := Start(t.Context(), req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if reply, err := session.Request(t.Context(), []byte("go")); err != nil || string(reply) != "ready" {
		t.Fatalf("handshake = %q, %v", reply, err)
	}
	started := time.Now()
	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(started); elapsed < sessionCloseGrace {
		t.Fatalf("Close took %v, want at least the %v grace period for a target that ignores SIGTERM", elapsed, sessionCloseGrace)
	}
	if got := countOpenFDs(t); got > fdsBefore {
		t.Errorf("fd leak: before=%d after=%d", fdsBefore, got)
	}
	if got := countAuraCgroups(t); got > cgroupsBefore {
		t.Errorf("cgroup leak: before=%d after=%d", cgroupsBefore, got)
	}
	requireNoProcess(t, "sleep 6047")
}

func requireNoProcess(t *testing.T, pattern string) {
	t.Helper()
	deadline := time.NewTimer(probeTimeout)
	defer deadline.Stop()
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	for {
		out, err := exec.CommandContext(t.Context(), "pgrep", "-f", pattern).Output()
		if err != nil {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("processes matching %q outlived the session: %s", pattern, out)
		case <-poll.C:
		}
	}
}

func TestIntegrationTeardownDoesNotWaitForATargetWithoutASIGTERMHandler(t *testing.T) {
	session := startHelperSession(t, nil)
	if got := exchange(t, session, "warm"); got != "warm" {
		t.Fatalf("warm exchange = %q", got)
	}
	started := time.Now()
	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= sessionCloseGrace {
		t.Fatalf("Close took %v for a target that SIGTERM would have ended, want under %v", elapsed, sessionCloseGrace)
	}
}
