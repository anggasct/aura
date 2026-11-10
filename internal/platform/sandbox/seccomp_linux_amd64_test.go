//go:build linux && amd64

package sandbox

import (
	"encoding/binary"
	"fmt"
	"slices"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

const (
	actionAllow = unix.SECCOMP_RET_ALLOW
	actionKill  = unix.SECCOMP_RET_KILL_PROCESS
)

func actionErrno(errno syscall.Errno) uint32 {
	return unix.SECCOMP_RET_ERRNO | uint32(errno)
}

type seccompCall struct {
	arch uint32
	nr   uint32
	args [6]uint64
}

func (c seccompCall) data() []byte {
	data := make([]byte, 64)
	binary.NativeEndian.PutUint32(data[seccompOffsetNr:], c.nr)
	binary.NativeEndian.PutUint32(data[seccompOffsetArch:], c.arch)
	for i, arg := range c.args {
		binary.NativeEndian.PutUint64(data[seccompOffsetArgs+seccompArgStride*i:], arg)
	}
	return data
}

func evaluateFilter(t *testing.T, filter []unix.SockFilter, call seccompCall) uint32 {
	t.Helper()
	data := call.data()
	var acc uint32
	for pc := 0; pc < len(filter); pc++ {
		ins := filter[pc]
		switch ins.Code {
		case unix.BPF_LD | unix.BPF_W | unix.BPF_ABS:
			acc = binary.NativeEndian.Uint32(data[ins.K:])
		case unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K:
			if acc == ins.K {
				pc += int(ins.Jt)
			} else {
				pc += int(ins.Jf)
			}
		case unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K:
			if acc&ins.K != 0 {
				pc += int(ins.Jt)
			} else {
				pc += int(ins.Jf)
			}
		case unix.BPF_RET | unix.BPF_K:
			return ins.K
		default:
			t.Fatalf("instruction %d: unsupported code %#x", pc, ins.Code)
		}
	}
	t.Fatal("filter fell off the end without a verdict")
	return 0
}

func compiledProfile(t *testing.T, profile Profile, pid uint32) []unix.SockFilter {
	t.Helper()
	resolved, err := buildProfile(profile, pid)
	if err != nil {
		t.Fatalf("buildProfile(%q): %v", profile, err)
	}
	filter, err := buildSeccompFilter(&resolved)
	if err != nil {
		t.Fatalf("buildSeccompFilter(%q): %v", profile, err)
	}
	return filter
}

func nativeCall(nr int, args ...uint64) seccompCall {
	call := seccompCall{arch: unix.AUDIT_ARCH_X86_64, nr: uint32(nr)}
	copy(call.args[:], args)
	return call
}

const testPid = 1

func TestSeccompFilterVerdicts(t *testing.T) {
	cases := []struct {
		name    string
		profile Profile
		call    seccompCall
		want    uint32
	}{
		{"base read", ProfileBase, nativeCall(unix.SYS_READ), actionAllow},
		{"base execve", ProfileBase, nativeCall(unix.SYS_EXECVE), actionAllow},
		{"base fork", ProfileBase, nativeCall(unix.SYS_FORK), actionAllow},
		{"base vfork", ProfileBase, nativeCall(unix.SYS_VFORK), actionAllow},
		{"base epoll_pwait stays out", ProfileBase, nativeCall(unix.SYS_EPOLL_PWAIT), actionKill},
		{"base eventfd2 stays out", ProfileBase, nativeCall(unix.SYS_EVENTFD2), actionKill},
		{"base prlimit64 on self", ProfileBase, nativeCall(unix.SYS_PRLIMIT64, 0), actionAllow},
		{"base prlimit64 on another pid", ProfileBase, nativeCall(unix.SYS_PRLIMIT64, 4242), actionKill},
		{"base prlimit64 pid 1", ProfileBase, nativeCall(unix.SYS_PRLIMIT64, 1), actionKill},
		{"base prlimit64 high word is not part of pid_t", ProfileBase, nativeCall(unix.SYS_PRLIMIT64, 1<<32), actionAllow},
		{"base fcntl F_GETFL", ProfileBase, nativeCall(unix.SYS_FCNTL, 3, unix.F_GETFL), actionAllow},
		{"base fcntl F_SETFL", ProfileBase, nativeCall(unix.SYS_FCNTL, 3, unix.F_SETFL, unix.O_ASYNC), actionAllow},
		{"base fcntl F_SETOWN", ProfileBase, nativeCall(unix.SYS_FCNTL, 3, unix.F_SETOWN, 4242), actionKill},
		{"base fcntl F_SETOWN_EX", ProfileBase, nativeCall(unix.SYS_FCNTL, 3, unix.F_SETOWN_EX, 0), actionKill},
		{"base fcntl F_SETOWN with the kernel-ignored high word set", ProfileBase, nativeCall(unix.SYS_FCNTL, 3, 1<<32|unix.F_SETOWN, 4242), actionKill},
		{"base fcntl F_SETSIG", ProfileBase, nativeCall(unix.SYS_FCNTL, 3, unix.F_SETSIG, uint64(unix.SIGKILL)), actionKill},
		{"base clone thread", ProfileBase, nativeCall(unix.SYS_CLONE, unix.CLONE_VM|unix.CLONE_FS|unix.CLONE_FILES|unix.CLONE_SIGHAND|unix.CLONE_THREAD|unix.CLONE_SYSVSEM), actionAllow},
		{"base clone with CLONE_NEWUSER and a high word", ProfileBase, nativeCall(unix.SYS_CLONE, 1<<40|unix.CLONE_NEWUSER), actionKill},
		{"base clone fork", ProfileBase, nativeCall(unix.SYS_CLONE, uint64(unix.SIGCHLD)), actionAllow},
		{"base prctl", ProfileBase, nativeCall(unix.SYS_PRCTL, unix.PR_SET_VMA), actionErrno(syscall.EPERM)},
		{"base tgkill on the own pid", ProfileBase, nativeCall(unix.SYS_TGKILL, testPid, 7, uint64(unix.SIGURG)), actionErrno(syscall.EPERM)},
		{"base tgkill on another pid", ProfileBase, nativeCall(unix.SYS_TGKILL, 4242, 4242, uint64(unix.SIGKILL)), actionErrno(syscall.EPERM)},
		{"base clone3", ProfileBase, nativeCall(unix.SYS_CLONE3), actionErrno(syscall.ENOSYS)},
		{"base unshare", ProfileBase, nativeCall(unix.SYS_UNSHARE, unix.CLONE_NEWUSER), actionKill},
		{"base setns", ProfileBase, nativeCall(unix.SYS_SETNS), actionKill},
		{"base kill", ProfileBase, nativeCall(unix.SYS_KILL, 4242, uint64(unix.SIGKILL)), actionKill},
		{"base tkill", ProfileBase, nativeCall(unix.SYS_TKILL, 4242, uint64(unix.SIGKILL)), actionKill},
		{"base socket", ProfileBase, nativeCall(unix.SYS_SOCKET), actionKill},
		{"base connect", ProfileBase, nativeCall(unix.SYS_CONNECT), actionKill},
		{"base ptrace", ProfileBase, nativeCall(unix.SYS_PTRACE), actionKill},
		{"base bpf", ProfileBase, nativeCall(unix.SYS_BPF), actionKill},
		{"base process_vm_writev", ProfileBase, nativeCall(unix.SYS_PROCESS_VM_WRITEV), actionKill},
		{"base pidfd_send_signal", ProfileBase, nativeCall(unix.SYS_PIDFD_SEND_SIGNAL), actionKill},
		{"base foreign arch", ProfileBase, seccompCall{arch: unix.AUDIT_ARCH_I386, nr: unix.SYS_READ}, actionKill},
		{"base x32 syscall number", ProfileBase, nativeCall(unix.SYS_READ | 0x40000000), actionKill},

		{"connector read", ProfileConnector, nativeCall(unix.SYS_READ), actionAllow},
		{"connector fork", ProfileConnector, nativeCall(unix.SYS_FORK), actionKill},
		{"connector vfork", ProfileConnector, nativeCall(unix.SYS_VFORK), actionKill},
		{"connector epoll_pwait", ProfileConnector, nativeCall(unix.SYS_EPOLL_PWAIT), actionAllow},
		{"connector eventfd2", ProfileConnector, nativeCall(unix.SYS_EVENTFD2), actionAllow},
		{"connector execve", ProfileConnector, nativeCall(unix.SYS_EXECVE), actionAllow},
		{"connector clone thread", ProfileConnector, nativeCall(unix.SYS_CLONE, unix.CLONE_VM|unix.CLONE_FS|unix.CLONE_FILES|unix.CLONE_SIGHAND|unix.CLONE_THREAD|unix.CLONE_SYSVSEM), actionAllow},
		{"connector clone without CLONE_THREAD", ProfileConnector, nativeCall(unix.SYS_CLONE, uint64(unix.SIGCHLD)), actionKill},
		{"connector clone thread with CLONE_NEWUSER", ProfileConnector, nativeCall(unix.SYS_CLONE, unix.CLONE_THREAD|unix.CLONE_NEWUSER), actionKill},
		{"connector clone3", ProfileConnector, nativeCall(unix.SYS_CLONE3), actionErrno(syscall.ENOSYS)},
		{"connector prctl", ProfileConnector, nativeCall(unix.SYS_PRCTL, unix.PR_SET_NAME), actionErrno(syscall.EPERM)},
		{"connector prctl seccomp", ProfileConnector, nativeCall(unix.SYS_PRCTL, unix.PR_SET_SECCOMP), actionErrno(syscall.EPERM)},
		{"connector tgkill on own pid", ProfileConnector, nativeCall(unix.SYS_TGKILL, testPid, 7, uint64(unix.SIGURG)), actionAllow},
		{"connector tgkill on another pid", ProfileConnector, nativeCall(unix.SYS_TGKILL, 4242, 4242, uint64(unix.SIGKILL)), actionKill},
		{"connector prlimit64 on another pid", ProfileConnector, nativeCall(unix.SYS_PRLIMIT64, 4242), actionKill},
		{"connector fcntl F_SETOWN", ProfileConnector, nativeCall(unix.SYS_FCNTL, 3, unix.F_SETOWN, 4242), actionKill},
		{"connector kill", ProfileConnector, nativeCall(unix.SYS_KILL, 4242, uint64(unix.SIGKILL)), actionKill},
		{"connector socket", ProfileConnector, nativeCall(unix.SYS_SOCKET), actionKill},
		{"connector connect", ProfileConnector, nativeCall(unix.SYS_CONNECT), actionKill},
		{"connector ptrace", ProfileConnector, nativeCall(unix.SYS_PTRACE), actionKill},
		{"connector bpf", ProfileConnector, nativeCall(unix.SYS_BPF), actionKill},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			filter := compiledProfile(t, tc.profile, testPid)
			if got := evaluateFilter(t, filter, tc.call); got != tc.want {
				t.Fatalf("verdict = %#x, want %#x", got, tc.want)
			}
		})
	}
}

func TestSeccompRejectsEveryNamespaceFlagOnClone(t *testing.T) {
	flags := map[string]uint64{
		"CLONE_NEWNS": unix.CLONE_NEWNS, "CLONE_NEWCGROUP": unix.CLONE_NEWCGROUP, "CLONE_NEWUTS": unix.CLONE_NEWUTS,
		"CLONE_NEWIPC": unix.CLONE_NEWIPC, "CLONE_NEWUSER": unix.CLONE_NEWUSER, "CLONE_NEWPID": unix.CLONE_NEWPID,
		"CLONE_NEWNET": unix.CLONE_NEWNET, "CLONE_NEWTIME": unix.CLONE_NEWTIME,
	}
	for _, profile := range []Profile{ProfileBase, ProfileConnector} {
		filter := compiledProfile(t, profile, testPid)
		for name, flag := range flags {
			t.Run(fmt.Sprintf("%s/%s", profile, name), func(t *testing.T) {
				call := nativeCall(unix.SYS_CLONE, flag|unix.CLONE_THREAD|uint64(unix.SIGCHLD))
				if got := evaluateFilter(t, filter, call); got != actionKill {
					t.Fatalf("verdict = %#x, want kill", got)
				}
			})
		}
	}
}

func TestSeccompPinsTgkillToTheProcessPid(t *testing.T) {
	filter := compiledProfile(t, ProfileConnector, 4321)
	if got := evaluateFilter(t, filter, nativeCall(unix.SYS_TGKILL, 4321, 4322, uint64(unix.SIGURG))); got != actionAllow {
		t.Fatalf("tgkill on the pinned pid = %#x, want allow", got)
	}
	if got := evaluateFilter(t, filter, nativeCall(unix.SYS_TGKILL, testPid, 4322, uint64(unix.SIGURG))); got != actionKill {
		t.Fatalf("tgkill on another pid = %#x, want kill", got)
	}
}

func TestSeccompProfilesAreDisjointAndBounded(t *testing.T) {
	for _, profile := range []Profile{ProfileBase, ProfileConnector} {
		t.Run(string(profile), func(t *testing.T) {
			resolved, err := buildProfile(profile, testPid)
			if err != nil {
				t.Fatalf("buildProfile: %v", err)
			}
			if _, err := buildSeccompFilter(&resolved); err != nil {
				t.Fatalf("buildSeccompFilter: %v", err)
			}
			if !slices.Contains(resolved.allow, unix.SYS_EXECVE) {
				t.Error("allowlist must include execve so the child can exec the tool")
			}
			duplicate := seccompProfile{allow: resolved.allow, rules: []seccompRule{failWith(resolved.allow[0], syscall.EPERM)}}
			if _, err := buildSeccompFilter(&duplicate); err == nil {
				t.Error("a syscall listed twice must fail the build")
			}
		})
	}
}

func TestBuildProfileRejectsUnknownProfiles(t *testing.T) {
	if _, err := buildProfile(Profile("unknown"), testPid); err == nil {
		t.Fatal("unknown profile must not build")
	}
}

func TestSeccompAllowlistExcludesNetworkAndDangerousSyscalls(t *testing.T) {
	for _, profile := range []Profile{ProfileBase, ProfileConnector} {
		resolved, err := buildProfile(profile, testPid)
		if err != nil {
			t.Fatalf("buildProfile: %v", err)
		}
		for _, denied := range []int{
			unix.SYS_SOCKET, unix.SYS_CONNECT, unix.SYS_BIND, unix.SYS_LISTEN, unix.SYS_ACCEPT,
			unix.SYS_SENDTO, unix.SYS_RECVFROM, unix.SYS_SETSOCKOPT,
			unix.SYS_MOUNT, unix.SYS_PTRACE, unix.SYS_BPF, unix.SYS_UNSHARE, unix.SYS_SETNS,
			unix.SYS_KILL, unix.SYS_TKILL, unix.SYS_OPEN,
		} {
			if slices.Contains(resolved.allow, denied) {
				t.Errorf("profile %q: syscall %d must not be on the allowlist", profile, denied)
			}
		}
	}
}
