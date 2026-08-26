//go:build linux && integration

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests exercise the fully confined Run path: the re-exec child runs in
// fresh user and network namespaces with Landlock, seccomp, rlimit, and cgroup
// enforcement. They require a kernel and runtime that allow an unprivileged
// binary to create user namespaces and to write cgroup controllers — run with
// `go test -tags=integration` in such an environment (e.g. as root, which
// bypasses AppArmor's unprivileged-userns restriction).

// TestMain intercepts the sandbox-child sentinel so the re-executed test
// binary runs RunChild instead of the test suite. The production aura binary
// dispatches the same sentinel from cmd/aura/main.go.
func TestMain(m *testing.M) {
	if IsChild(os.Args) {
		os.Exit(RunChild())
	}
	os.Exit(m.Run())
}

func baseSpec(t *testing.T) Spec {
	t.Helper()
	return Spec{
		WorkingDir: t.TempDir(),
		AllowEnv:   []string{"PATH=" + os.Getenv("PATH")},
		Limits:     Limits{Timeout: 10 * time.Second},
	}
}

func TestIntegrationRunBasicOutput(t *testing.T) {
	spec := baseSpec(t)
	result, err := Run(context.Background(), &spec, "printf", "hello-sandbox")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 || result.Output != "hello-sandbox" {
		t.Fatalf("result=%+v, want exit 0 output hello-sandbox", result)
	}
}

func TestIntegrationEnvAllowlistExcludesParentSecrets(t *testing.T) {
	t.Setenv("AURA_SANDBOX_CANARY", "canary-zz9-6k2")
	spec := baseSpec(t)
	result, err := Run(context.Background(), &spec, "env")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(result.Output, "AURA_SANDBOX_CANARY") || strings.Contains(result.Output, "canary-zz9-6k2") {
		t.Fatalf("canary leaked into child:\n%s", result.Output)
	}
}

func TestIntegrationWorkingDirConfined(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	spec := baseSpec(t)
	spec.WorkingDir = dir
	result, err := Run(context.Background(), &spec, "sh", "-c", "test -f marker.txt && pwd")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit=%d out=%q", result.ExitCode, result.Output)
	}
	if strings.TrimSpace(result.Output) != dir {
		t.Fatalf("pwd=%q want %q", result.Output, dir)
	}
}

func TestIntegrationKillsWholeProcessGroupOnTimeout(t *testing.T) {
	spec := baseSpec(t)
	spec.Limits.Timeout = 400 * time.Millisecond
	result, err := Run(context.Background(), &spec, "sh", "-c", "sleep 60 & sleep 60")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Terminated {
		t.Fatalf("Terminated=false (exit=%d)", result.ExitCode)
	}
}

func TestIntegrationCancelKillsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	spec := baseSpec(t)
	spec.Limits.Timeout = 30 * time.Second
	done := make(chan struct{})
	go func() {
		defer close(done)
		result, err := Run(ctx, &spec, "sleep", "60")
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		if !result.Terminated {
			t.Errorf("Terminated=false")
		}
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestIntegrationOutputCapped(t *testing.T) {
	spec := baseSpec(t)
	spec.Limits.MaxOutputBytes = 16
	result, err := Run(context.Background(), &spec, "sh", "-c", "printf '0123456789abcdefGHIJKLMNOP'")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Truncated || len(result.Output) != 16 {
		t.Fatalf("Truncated=%v len=%d", result.Truncated, len(result.Output))
	}
}

// Landlock denies a read outside the declared roots. The workspace is
// declared read-write; a sibling directory outside it is not, so a tool that
// reads a file there must fail.
func TestIntegrationLandlockDeniesEscape(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("treasure"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	spec := baseSpec(t)
	result, _ := Run(context.Background(), &spec, "cat", secret)
	if strings.Contains(result.Output, "treasure") {
		t.Fatalf("Landlock escape: child read outside-declared file (%+v)", result)
	}
	if result.ExitCode == 0 {
		t.Fatalf("Landlock escape: cat succeeded outside declared roots")
	}
}

// An rlimit file-size bound kills a tool that writes past it. The writing
// command is the whole script so its signal exit is the run's exit.
func TestIntegrationRlimitFileBytes(t *testing.T) {
	spec := baseSpec(t)
	spec.Limits.FileBytes = 16
	result, err := Run(context.Background(), &spec, "sh", "-c", "yes | head -c 4096 > big")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode == 0 {
		t.Fatalf("rlimit FSIZE not enforced: child wrote past the bound (%+v)", result)
	}
}

// TestIntegrationInitFailureDeterministic triggers a child setup failure
// repeatedly and asserts the typed sandbox_init_failed error is returned every
// time, with no goroutine left blocked on the coordination path.
func TestIntegrationInitFailureDeterministic(t *testing.T) {
	spec := baseSpec(t)
	spec.ReadOnlyPaths = []string{"/nonexistent-sandbox-probe-path"}
	before := runtime.NumGoroutine()
	for range 5 {
		_, err := Run(context.Background(), &spec, "true")
		if code, ok := CodeOf(err); !ok || code != ErrorCodeSandboxInitFailed {
			t.Fatalf("Run = %v, want sandbox_init_failed", err)
		}
	}
	// Give the reaped goroutines a moment to exit, then assert no growth.
	time.Sleep(100 * time.Millisecond)
	if got := runtime.NumGoroutine(); got > before+1 {
		t.Fatalf("goroutine leak: before=%d after=%d", before, got)
	}
}

// A symlink inside the workspace pointing at an outside file cannot
// smuggle access to it; Landlock follows the link to its undeclared target.
func TestIntegrationLandlockDeniesSymlinkEscape(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("treasure"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	dir := t.TempDir()
	if err := os.Symlink(secret, filepath.Join(dir, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	spec := baseSpec(t)
	spec.WorkingDir = dir
	result, _ := Run(context.Background(), &spec, "cat", filepath.Join(dir, "link"))
	if strings.Contains(result.Output, "treasure") {
		t.Fatalf("symlink escape: child read outside file via link (%+v)", result)
	}
	if result.ExitCode == 0 {
		t.Fatalf("symlink escape: cat succeeded outside declared roots")
	}
}

// An rlimit CPU bound kills a tool that burns past it.
func TestIntegrationRlimitCPUTime(t *testing.T) {
	spec := baseSpec(t)
	spec.Limits.CPUTime = 1 * time.Second
	result, err := Run(context.Background(), &spec, "sh", "-c", "while :; do :; done")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode == 0 && !result.Terminated {
		t.Fatalf("rlimit CPU not enforced: child ran past the bound (%+v)", result)
	}
}

// A cgroup memory bound kills a tool that allocates past it.
func TestIntegrationCgroupMemory(t *testing.T) {
	if !cgroupControllersWritable() {
		t.Skip("cgroup controllers not delegated on this host")
	}
	spec := baseSpec(t)
	spec.Limits.MemoryBytes = 4 << 20
	result, err := Run(context.Background(), &spec, "sh", "-c", "x=$(seq 3000000); echo ${#x}")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode == 0 && !result.Terminated {
		t.Fatalf("cgroup memory not enforced: child allocated past the bound (%+v)", result)
	}
}

// A child that attempts a network connection is denied. The seccomp
// filter excludes socket syscalls and the network namespace has no external
// interface, so bash's /dev/tcp open must fail and kill the child.
func TestIntegrationNetworkDenied(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available for the network-attempt fixture")
	}
	spec := baseSpec(t)
	result, _ := Run(context.Background(), &spec, "bash", "-c", "echo > /dev/tcp/203.0.113.1/1")
	if result.ExitCode == 0 {
		t.Fatalf("network not denied: child opened an external connection (%+v)", result)
	}
}

// An rlimit open-files bound is enforced: opening past MaxOpenFiles fails.
func TestIntegrationRlimitOpenFiles(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available for the open-files fixture")
	}
	spec := baseSpec(t)
	spec.Limits.MaxOpenFiles = 9
	result, _ := Run(context.Background(), &spec, "bash", "-c", "for i in {1..100}; do exec {fd}>f; done")
	if result.ExitCode == 0 {
		t.Fatalf("rlimit NOFILE not enforced: child opened past the bound (%+v)", result)
	}
}

// A cgroup PID bound is enforced: a fork-heavy child cannot spawn past
// MaxProcesses. Env-gated like the memory test (needs delegated controllers).
func TestIntegrationCgroupPids(t *testing.T) {
	if !cgroupControllersWritable() {
		t.Skip("cgroup controllers not delegated on this host")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available for the PID-exhaustion fixture")
	}
	spec := baseSpec(t)
	spec.Limits.MaxProcesses = 8
	spec.Limits.MaxOutputBytes = 1 << 20
	result, _ := Run(context.Background(), &spec, "bash", "-c", "for i in $(seq 1 200); do : & done; wait")
	// pids.max makes fork fail with EAGAIN; bash surfaces it on stderr.
	if !strings.Contains(result.Output, "Resource temporarily unavailable") && result.ExitCode == 0 {
		t.Fatalf("cgroup pids not enforced: child forked past the bound (%+v)", result)
	}
}

// A parent file descriptor opened without close-on-exec would otherwise survive
// execve into the confined child. The child closes every descriptor above the
// config and init-error pipes, so the secret held by the leaked fd is
// unreadable from inside the sandbox.
func TestIntegrationFDNoLeak(t *testing.T) {
	tmp := t.TempDir()
	secretPath := filepath.Join(tmp, "secret")
	if err := os.WriteFile(secretPath, []byte("fd-canary-SECRET"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	fd, err := syscall.Open(secretPath, syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer syscall.Close(fd)
	const target = 9
	if err := syscall.Dup3(fd, target, 0); err != nil {
		t.Fatalf("dup3: %v", err)
	}
	syscall.Close(fd)
	defer syscall.Close(target)
	spec := baseSpec(t)
	result, err := Run(context.Background(), &spec, "sh", "-c", fmt.Sprintf("cat <&%d", target))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(result.Output, "fd-canary-SECRET") {
		t.Fatalf("fd leaked secret into child:\n%s", result.Output)
	}
}

// An elevated request bound to a registered grant executes inside the
// containment contract.
func TestIntegrationElevatedRunApproved(t *testing.T) {
	req := testRequest(t)
	req.Executable = "printf"
	req.Arguments = []string{"approved-run"}
	// This test proves the approved request executes, not memory enforcement
	// (covered separately below). The re-executed runner binary's early
	// resident set varies with platform and page-cache charging, and a tight
	// cgroup ceiling can OOM-kill it on cold hosted runners before the
	// target ever runs.
	req.Limits.MemoryBytes = 0
	grant := mintTestGrant(t, "v1", time.Minute, req)
	registry := NewRegistry("v1", nil)
	if err := registry.Register(context.Background(), &grant, req); err != nil {
		t.Fatalf("Register: %v", err)
	}
	req.ApprovalGrantID = grant.GrantID
	result, err := registry.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ExitCode != 0 || !strings.Contains(result.Output, "approved-run") {
		t.Fatalf("result=%+v", result)
	}
}

// A request altered after approval is refused before any child starts, so the
// contained executor never runs a tampered request.
func TestIntegrationElevatedRunRejectsMutation(t *testing.T) {
	req := testRequest(t)
	req.Executable = "printf"
	req.Arguments = []string{"approved-run"}
	grant := mintTestGrant(t, "v1", time.Minute, req)
	registry := NewRegistry("v1", nil)
	if err := registry.Register(context.Background(), &grant, req); err != nil {
		t.Fatalf("Register: %v", err)
	}
	mutated := cloneRequest(req)
	mutated.ApprovalGrantID = grant.GrantID
	mutated.Arguments = []string{"tampered"}
	_, err := registry.Execute(context.Background(), mutated)
	if code, ok := CodeOf(err); !ok || code != ErrorCodeApprovalInvalid {
		t.Fatalf("Execute mutated = %v, want approval_invalid", err)
	}
}

// Repeated cancellation must not leak goroutines, file descriptors, cgroup
// subtrees, or orphaned processes. Each iteration launches a long-lived child
// and cancels it mid-run; after the sweep the parent's resource usage is back
// at the baseline.
func TestIntegrationCancelNoLeak(t *testing.T) {
	goroutinesBefore := runtime.NumGoroutine()
	fdsBefore := countOpenFDs(t)
	cgroupBefore := countAuraCgroups(t)

	for range 5 {
		ctx, cancel := context.WithCancel(context.Background())
		spec := baseSpec(t)
		spec.Limits.Timeout = 30 * time.Second
		go func() {
			_, _ = Run(ctx, &spec, "sleep", "300")
		}()
		time.Sleep(150 * time.Millisecond)
		cancel()
	}
	time.Sleep(500 * time.Millisecond)

	if got := runtime.NumGoroutine(); got > goroutinesBefore+2 {
		t.Errorf("goroutine leak: before=%d after=%d", goroutinesBefore, got)
	}
	if got := countOpenFDs(t); got > fdsBefore+2 {
		t.Errorf("fd leak: before=%d after=%d", fdsBefore, got)
	}
	if got := countAuraCgroups(t); got > cgroupBefore {
		t.Errorf("cgroup leak: before=%d after=%d", cgroupBefore, got)
	}
	if orphans := countSleepOrphans(); orphans != 0 {
		t.Errorf("orphan sleep processes: %d", orphans)
	}
}

func countOpenFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("cannot read /proc/self/fd: %v", err)
	}
	return len(entries)
}

func countAuraCgroups(t *testing.T) int {
	t.Helper()
	parent, err := ownCgroupPath()
	if err != nil {
		t.Skipf("cannot resolve own cgroup: %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Skipf("cannot read cgroup parent: %v", err)
	}
	count := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "aura-sandbox-") {
			count++
		}
	}
	return count
}

func countSleepOrphans() int {
	out, err := exec.Command("pgrep", "-c", "-f", "sleep 300").Output()
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0
	}
	return n
}
