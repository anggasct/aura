//go:build linux

package sandbox

import (
	"context"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

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
