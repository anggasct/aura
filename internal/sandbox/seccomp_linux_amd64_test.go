//go:build linux && amd64

package sandbox

import (
	"slices"
	"testing"

	"golang.org/x/sys/unix"
)

func TestBuildSeccompFilter(t *testing.T) {
	allowed := []int{unix.SYS_READ, unix.SYS_WRITE, unix.SYS_EXECVE}
	f := buildSeccompFilter(allowed)

	// [load arch, arch==x86_64?, kill, load nr, 3 comparisons, default kill, allow]
	if len(f) != len(allowed)+6 {
		t.Fatalf("filter length = %d, want %d", len(f), len(allowed)+6)
	}
	if f[0].Code != unix.BPF_LD|unix.BPF_W|unix.BPF_ABS || f[0].K != seccompOffsetArch {
		t.Errorf("first instruction does not load arch: %+v", f[0])
	}
	if f[1].K != unix.AUDIT_ARCH_X86_64 {
		t.Errorf("arch comparison not against x86_64: K=%d", f[1].K)
	}
	if f[2].Code != unix.BPF_RET|unix.BPF_K || f[2].K != unix.SECCOMP_RET_KILL_PROCESS {
		t.Errorf("arch-mismatch fallthrough not kill_process: %+v", f[2])
	}

	// Each allowed syscall comparison must jump to ALLOW on a match.
	allowIdx := len(f) - 1
	for offset, want := range allowed {
		ins := f[4+offset]
		if ins.K != uint32(want) {
			t.Errorf("comparison %d K=%d, want syscall %d", offset, ins.K, want)
		}
		if 4+offset+int(ins.Jt)+1 != allowIdx {
			t.Errorf("comparison %d Jt=%d does not land on ALLOW", offset, ins.Jt)
		}
	}

	if f[len(f)-2].K != unix.SECCOMP_RET_KILL_PROCESS {
		t.Errorf("default fallthrough not kill_process: %+v", f[len(f)-2])
	}
	if f[allowIdx].K != unix.SECCOMP_RET_ALLOW {
		t.Errorf("final instruction not allow: %+v", f[allowIdx])
	}
}

func TestAllowedSyscallsExcludesNetworkAndDangerous(t *testing.T) {
	allowed := allowedSyscalls()
	if !slices.Contains(allowed, unix.SYS_EXECVE) {
		t.Error("allowlist must include execve so the child can exec the tool")
	}
	for _, denied := range []int{
		unix.SYS_SOCKET, unix.SYS_CONNECT, unix.SYS_BIND, unix.SYS_LISTEN, unix.SYS_ACCEPT,
		unix.SYS_SENDTO, unix.SYS_RECVFROM, unix.SYS_SETSOCKOPT,
		unix.SYS_MOUNT, unix.SYS_PTRACE, unix.SYS_BPF, unix.SYS_UNSHARE, unix.SYS_SETNS,
		unix.SYS_OPEN, // legacy open is not on the allowlist; openat is
	} {
		if slices.Contains(allowed, denied) {
			t.Errorf("syscall %d must not be on the allowlist", denied)
		}
	}
}
