//go:build linux && amd64

package sandbox

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"slices"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func filterDigest(filter []unix.SockFilter) string {
	digest := sha256.New()
	for _, ins := range filter {
		var word [8]byte
		binary.LittleEndian.PutUint16(word[0:], ins.Code)
		word[2], word[3] = ins.Jt, ins.Jf
		binary.LittleEndian.PutUint32(word[4:], ins.K)
		digest.Write(word[:])
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func TestBaseAndConnectorFiltersAreUnchanged(t *testing.T) {
	pinned := map[Profile]string{
		ProfileBase:      "801b34d48c6ca14017006f201e26e5284842547e56f952e3d14c6c45cab42e34",
		ProfileConnector: "e280ed102835c16dc74b2d2a512ceb20d55d970930aaf3f8eb788168ad94eb3a",
	}
	for profile, want := range pinned {
		t.Run(fmt.Sprintf("%q", profile), func(t *testing.T) {
			if got := filterDigest(compiledProfile(t, profile, testPid)); got != want {
				t.Fatalf("filter digest = %s, want %s", got, want)
			}
		})
	}
}

var interpreterAdditionRuntimes = map[int]struct {
	name    string
	runtime string
}{
	unix.SYS_CAPGET:             {"capget", "javascript"},
	unix.SYS_EPOLL_PWAIT:        {"epoll_pwait", "javascript"},
	unix.SYS_EVENTFD2:           {"eventfd2", "javascript"},
	unix.SYS_GETSOCKNAME:        {"getsockname", "python"},
	unix.SYS_GETPEERNAME:        {"getpeername", "python"},
	unix.SYS_GETSOCKOPT:         {"getsockopt", "python"},
	unix.SYS_SCHED_GETSCHEDULER: {"sched_getscheduler", "javascript"},
	unix.SYS_SCHED_GETPARAM:     {"sched_getparam", "javascript"},
	unix.SYS_SENDTO:             {"sendto", "python"},
	unix.SYS_RECVFROM:           {"recvfrom", "python"},
	unix.SYS_MKDIR:              {"mkdir", "python"},
	unix.SYS_RMDIR:              {"rmdir", "python"},
	unix.SYS_UNLINK:             {"unlink", "python"},
	unix.SYS_UNLINKAT:           {"unlinkat", "python"},
	unix.SYS_RENAME:             {"rename", "python"},
	unix.SYS_RENAMEAT:           {"renameat", "python"},
	unix.SYS_RENAMEAT2:          {"renameat2", "python"},
	unix.SYS_FSYNC:              {"fsync", "python"},
	unix.SYS_SENDFILE:           {"sendfile", "python"},
	unix.SYS_TGKILL:             {"tgkill", "python"},
}

func TestInterpreterAdditionsNameTheRuntimeThatNeedsEach(t *testing.T) {
	filter := compiledProfile(t, ProfileInterpreter, testPid)
	base := compiledProfile(t, ProfileBase, testPid)
	for nr, entry := range interpreterAdditionRuntimes {
		t.Run(entry.name+"/"+entry.runtime, func(t *testing.T) {
			if got := evaluateFilter(t, filter, nativeCall(nr)); got != actionAllow {
				t.Fatalf("interpreter verdict = %#x, want allow", got)
			}
			if got := evaluateFilter(t, base, nativeCall(nr)); got == actionAllow {
				t.Fatalf("base verdict = allow, the addition must not leak into the base profile")
			}
		})
	}
	t.Run("the table matches the additions exactly", func(t *testing.T) {
		var listed []int
		for nr := range interpreterAdditionRuntimes {
			listed = append(listed, nr)
		}
		added := interpreterAdditions()
		slices.Sort(listed)
		slices.Sort(added)
		if !slices.Equal(listed, added) {
			t.Fatalf("documented additions %v differ from the profile additions %v", listed, added)
		}
	})
}

func TestInterpreterProfileVerdicts(t *testing.T) {
	af := func(domain uint64) seccompCall { return nativeCall(unix.SYS_SOCKETPAIR, domain) }
	cases := []struct {
		name string
		call seccompCall
		want uint32
	}{
		{"read", nativeCall(unix.SYS_READ), actionAllow},
		{"openat", nativeCall(unix.SYS_OPENAT), actionAllow},
		{"execve", nativeCall(unix.SYS_EXECVE), actionAllow},
		{"fork", nativeCall(unix.SYS_FORK), actionAllow},
		{"socket", nativeCall(unix.SYS_SOCKET, unix.AF_INET), actionErrno(syscall.EACCES)},
		{"connect", nativeCall(unix.SYS_CONNECT), actionErrno(syscall.EACCES)},
		{"bind", nativeCall(unix.SYS_BIND), actionErrno(syscall.EACCES)},
		{"listen", nativeCall(unix.SYS_LISTEN), actionKill},
		{"accept", nativeCall(unix.SYS_ACCEPT), actionKill},
		{"setsockopt", nativeCall(unix.SYS_SETSOCKOPT), actionKill},
		{"socketpair AF_UNIX", af(unix.AF_UNIX), actionAllow},
		{"socketpair AF_INET", af(unix.AF_INET), actionKill},
		{"socketpair AF_NETLINK", af(unix.AF_NETLINK), actionKill},
		{"io_uring_setup", nativeCall(unix.SYS_IO_URING_SETUP), actionErrno(syscall.ENOSYS)},
		{"io_uring_enter", nativeCall(unix.SYS_IO_URING_ENTER), actionKill},
		{"pidfd_open", nativeCall(unix.SYS_PIDFD_OPEN), actionErrno(syscall.ENOSYS)},
		{"pidfd_send_signal", nativeCall(unix.SYS_PIDFD_SEND_SIGNAL), actionErrno(syscall.ENOSYS)},
		{"clone3", nativeCall(unix.SYS_CLONE3), actionErrno(syscall.ENOSYS)},
		{"prctl", nativeCall(unix.SYS_PRCTL, unix.PR_SET_NAME), actionErrno(syscall.EPERM)},
		{"prctl seccomp", nativeCall(unix.SYS_PRCTL, unix.PR_SET_SECCOMP), actionErrno(syscall.EPERM)},
		{"tgkill on the own pid", nativeCall(unix.SYS_TGKILL, testPid, 7, uint64(unix.SIGURG)), actionAllow},
		{"tgkill on another pid", nativeCall(unix.SYS_TGKILL, 4242, 4242, uint64(unix.SIGKILL)), actionAllow},
		{"kill", nativeCall(unix.SYS_KILL, 4242, uint64(unix.SIGKILL)), actionKill},
		{"tkill", nativeCall(unix.SYS_TKILL, 4242, uint64(unix.SIGKILL)), actionKill},
		{"clone thread", nativeCall(unix.SYS_CLONE, unix.CLONE_VM|unix.CLONE_FS|unix.CLONE_FILES|unix.CLONE_SIGHAND|unix.CLONE_THREAD|unix.CLONE_SYSVSEM), actionAllow},
		{"clone fork", nativeCall(unix.SYS_CLONE, uint64(unix.SIGCHLD)), actionAllow},
		{"clone with CLONE_NEWUSER", nativeCall(unix.SYS_CLONE, unix.CLONE_NEWUSER), actionKill},
		{"clone with CLONE_NEWNET", nativeCall(unix.SYS_CLONE, unix.CLONE_THREAD|unix.CLONE_NEWNET), actionKill},
		{"clone with CLONE_NEWPID", nativeCall(unix.SYS_CLONE, unix.CLONE_NEWPID), actionKill},
		{"unshare", nativeCall(unix.SYS_UNSHARE, unix.CLONE_NEWUSER), actionKill},
		{"setns", nativeCall(unix.SYS_SETNS), actionKill},
		{"ptrace", nativeCall(unix.SYS_PTRACE), actionKill},
		{"mount", nativeCall(unix.SYS_MOUNT), actionKill},
		{"bpf", nativeCall(unix.SYS_BPF), actionKill},
		{"setxattr", nativeCall(unix.SYS_SETXATTR), actionKill},
		{"open", nativeCall(unix.SYS_OPEN), actionKill},
		{"prlimit64 on another pid", nativeCall(unix.SYS_PRLIMIT64, 4242), actionKill},
		{"fcntl F_SETOWN", nativeCall(unix.SYS_FCNTL, 3, unix.F_SETOWN, 4242), actionKill},
		{"foreign arch", seccompCall{arch: unix.AUDIT_ARCH_I386, nr: unix.SYS_READ}, actionKill},
		{"x32 syscall number", nativeCall(unix.SYS_READ | 0x40000000), actionKill},
	}
	filter := compiledProfile(t, ProfileInterpreter, testPid)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := evaluateFilter(t, filter, tc.call); got != tc.want {
				t.Fatalf("verdict = %#x, want %#x", got, tc.want)
			}
		})
	}
}

func TestInterpreterProfileDeniesMetadataChanges(t *testing.T) {
	filter := compiledProfile(t, ProfileInterpreter, testPid)
	resolved, err := buildProfile(ProfileInterpreter, testPid)
	if err != nil {
		t.Fatal(err)
	}
	names := map[int]string{
		unix.SYS_CHMOD: "chmod", unix.SYS_FCHMOD: "fchmod", unix.SYS_FCHMODAT: "fchmodat", unix.SYS_FCHMODAT2: "fchmodat2",
		unix.SYS_CHOWN: "chown", unix.SYS_FCHOWN: "fchown", unix.SYS_LCHOWN: "lchown", unix.SYS_FCHOWNAT: "fchownat",
		unix.SYS_UTIME: "utime", unix.SYS_UTIMES: "utimes", unix.SYS_FUTIMESAT: "futimesat", unix.SYS_UTIMENSAT: "utimensat",
	}
	if len(names) != len(metadataSyscalls()) {
		t.Fatalf("the table names %d syscalls, the profile denies %d", len(names), len(metadataSyscalls()))
	}
	for nr, name := range names {
		t.Run(name, func(t *testing.T) {
			if !slices.Contains(metadataSyscalls(), nr) {
				t.Fatalf("%s is missing from the denied metadata family", name)
			}
			if slices.Contains(resolved.allow, nr) {
				t.Fatalf("%s is still on the interpreter allow list", name)
			}
			if got := evaluateFilter(t, filter, nativeCall(nr)); got != actionErrno(syscall.EPERM) {
				t.Fatalf("verdict = %#x, want EPERM", got)
			}
		})
	}
}

func TestBaseProfileKeepsItsMetadataSyscalls(t *testing.T) {
	filter := compiledProfile(t, ProfileBase, testPid)
	for _, nr := range []int{unix.SYS_FCHMOD, unix.SYS_FCHMODAT, unix.SYS_FCHOWN, unix.SYS_FCHOWNAT} {
		if got := evaluateFilter(t, filter, nativeCall(nr)); got != actionAllow {
			t.Errorf("base verdict for syscall %d = %#x, want allow", nr, got)
		}
	}
}

func TestInterpreterFilterFitsTheJumpRange(t *testing.T) {
	resolved, err := buildProfile(ProfileInterpreter, testPid)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.allow) > seccompMaxJump {
		t.Fatalf("the allow list has %d entries, the jump range is %d", len(resolved.allow), seccompMaxJump)
	}
	if _, err := buildSeccompFilter(&resolved); err != nil {
		t.Fatalf("buildSeccompFilter: %v", err)
	}
	for _, denied := range []int{unix.SYS_SOCKET, unix.SYS_CONNECT, unix.SYS_BIND, unix.SYS_KILL, unix.SYS_TKILL, unix.SYS_PTRACE, unix.SYS_MOUNT} {
		if slices.Contains(resolved.allow, denied) {
			t.Errorf("syscall %d must not be on the interpreter allow list", denied)
		}
	}
}
