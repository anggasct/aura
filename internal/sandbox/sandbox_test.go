//go:build linux

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestRunBasicOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	spec := Spec{
		WorkingDir: t.TempDir(),
		AllowEnv:   []string{"PATH=" + os.Getenv("PATH")},
		Limits:     Limits{Timeout: 5 * time.Second},
	}
	result, err := Run(ctx, &spec, "printf", "hello-sandbox")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0", result.ExitCode)
	}
	if result.Output != "hello-sandbox" {
		t.Fatalf("output = %q", result.Output)
	}
}

// Process-group termination: a child that forks a grandchild and outlives
// the deadline must be killed together with its descendants.
func TestRunKillsWholeProcessGroupOnTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	spec := Spec{
		WorkingDir: t.TempDir(),
		AllowEnv:   []string{"PATH=" + os.Getenv("PATH")},
		Limits:     Limits{Timeout: 300 * time.Millisecond},
	}
	// sh spawns a background grandchild, then sleeps; killing the group
	// must take both down, and Run must report Terminated.
	result, err := Run(ctx, &spec, "sh", "-c", "sleep 60 & sleep 60")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Terminated {
		t.Fatalf("result.Terminated = false, want true (exit=%d)", result.ExitCode)
	}
}

func TestRunCancelKillsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	spec := Spec{
		WorkingDir: t.TempDir(),
		AllowEnv:   []string{"PATH=" + os.Getenv("PATH")},
		Limits:     Limits{Timeout: 30 * time.Second},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		result, err := Run(ctx, &spec, "sleep", "60")
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		if !result.Terminated {
			t.Errorf("Terminated = false, want true")
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

// Env allowlist: only the declared variables reach the child, and a secret
// canary from the parent environment never leaks in.
func TestRunEnvAllowlistExcludesParentSecrets(t *testing.T) {
	t.Setenv("AURA_SANDBOX_CANARY", "canary-zz9-6k2")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	spec := Spec{
		WorkingDir: t.TempDir(),
		AllowEnv:   []string{"PATH=" + os.Getenv("PATH")},
		Limits:     Limits{Timeout: 5 * time.Second},
	}
	result, err := Run(ctx, &spec, "env")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(result.Output, "AURA_SANDBOX_CANARY") || strings.Contains(result.Output, "canary-zz9-6k2") {
		t.Fatalf("canary leaked into child environment:\n%s", result.Output)
	}
	if !strings.Contains(result.Output, "PATH=") {
		t.Fatalf("allowlisted PATH missing from child environment:\n%s", result.Output)
	}
}

// Working-dir confinement: the child sees the declared directory as its
// current directory.
func TestRunWorkingDirConfined(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	spec := Spec{
		WorkingDir: dir,
		AllowEnv:   []string{"PATH=" + os.Getenv("PATH")},
		Limits:     Limits{Timeout: 5 * time.Second},
	}
	result, err := Run(ctx, &spec, "sh", "-c", "test -f marker.txt && pwd")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("marker not visible from working dir (exit=%d, out=%q)", result.ExitCode, result.Output)
	}
	if strings.TrimSpace(result.Output) != dir {
		t.Fatalf("pwd = %q, want %q", result.Output, dir)
	}
}

func TestRunNetworkDeniedByDefault(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	spec := Spec{
		WorkingDir:   t.TempDir(),
		AllowEnv:     []string{"PATH=" + os.Getenv("PATH")},
		AllowNetwork: true,
		Limits:       Limits{Timeout: 5 * time.Second},
	}
	_, err := Run(ctx, &spec, "true")
	if code, ok := CodeOf(err); !ok || code != ErrorCodeSandboxUnavailable {
		t.Fatalf("Run(AllowNetwork) = %v, want sandbox_unavailable", err)
	}
}

func TestRunInvalidSpecFailsClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Run(ctx, &Spec{Limits: Limits{Timeout: time.Second}}, "true")
	if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
		t.Fatalf("Run(empty workdir) = %v, want invalid_argument", err)
	}
	_, err = Run(ctx, &Spec{WorkingDir: t.TempDir()}, "true")
	if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
		t.Fatalf("Run(zero timeout) = %v, want invalid_argument", err)
	}
}

func TestRunOutputCapped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	spec := Spec{
		WorkingDir: t.TempDir(),
		AllowEnv:   []string{"PATH=" + os.Getenv("PATH")},
		Limits:     Limits{Timeout: 5 * time.Second, MaxOutputBytes: 16},
	}
	result, err := Run(ctx, &spec, "sh", "-c", "printf '0123456789abcdefGHIJKLMNOP'")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Truncated {
		t.Fatalf("Truncated = false, want true (output=%q)", result.Output)
	}
	if len(result.Output) != 16 {
		t.Fatalf("output length = %d, want 16 (output=%q)", len(result.Output), result.Output)
	}
}

func TestRunOutputNotCappedWhenUnderLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	spec := Spec{
		WorkingDir: t.TempDir(),
		AllowEnv:   []string{"PATH=" + os.Getenv("PATH")},
		Limits:     Limits{Timeout: 5 * time.Second, MaxOutputBytes: 1 << 20},
	}
	result, err := Run(ctx, &spec, "printf", "short")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Truncated {
		t.Fatal("Truncated = true for small output")
	}
	if result.Output != "short" {
		t.Fatalf("output = %q", result.Output)
	}
}

func TestNegotiateReportsPrimitives(t *testing.T) {
	primitives, err := Negotiate()
	if err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	if !primitives.ProcessGroups {
		t.Fatal("process groups must always be available on Linux")
	}
	// Probe the kernel independently (raw syscall with arch-aware constant)
	// and require Negotiate to agree — this catches arch-specific syscall
	// number bugs that a tautological comparison would miss.
	action := unix.SECCOMP_RET_ALLOW
	_, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_GET_ACTION_AVAIL, 0, uintptr(unsafe.Pointer(&action)))
	want := errno != syscall.ENOSYS
	if primitives.Seccomp != want {
		t.Fatalf("Negotiate seccomp=%v, kernel reports %v", primitives.Seccomp, want)
	}
	if primitives.UserNamespace != usernsExpected() {
		t.Fatalf("Negotiate userns=%v, sysctl reports %v", primitives.UserNamespace, usernsExpected())
	}
	t.Logf("primitives: userns=%v seccomp=%v cgroupv2=%v landlock=%v", primitives.UserNamespace, primitives.Seccomp, primitives.CgroupV2, primitives.Landlock)
}

// usernsExpected is an independent read of the kernel knobs so the test does
// not just re-run the implementation. It mirrors what an operator checking
// unprivileged-userns support by hand would read.
func usernsExpected() bool {
	data, err := os.ReadFile("/proc/sys/user/max_user_namespaces")
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || n <= 0 {
		return false
	}
	if gate, err := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone"); err == nil && strings.TrimSpace(string(gate)) == "0" {
		return false
	}
	return true
}
