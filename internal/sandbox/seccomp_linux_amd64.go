//go:build linux && amd64

package sandbox

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func seccompAvailable() bool {
	action := unix.SECCOMP_RET_ALLOW
	_, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_GET_ACTION_AVAIL, 0, uintptr(unsafe.Pointer(&action)))
	return errno != syscall.ENOSYS
}

// allowedSyscalls is the amd64 allowlist a contained tool may issue. It covers
// dynamic-loader setup, ordinary file and memory operations, signals, time,
// process/thread lifecycle, and execve. Network syscalls, namespace creation
// (unshare/setns), credential changes, module loading, bpf, ptrace, and other
// admin/debug surfaces are intentionally absent: the BPF filter kills any
// syscall not on this list.
func allowedSyscalls() []int {
	return []int{
		unix.SYS_READ, unix.SYS_WRITE, unix.SYS_OPENAT, unix.SYS_CLOSE, unix.SYS_CLOSE_RANGE,
		unix.SYS_LSEEK, unix.SYS_PREAD64, unix.SYS_PWRITE64, unix.SYS_READV, unix.SYS_WRITEV,
		unix.SYS_PREADV, unix.SYS_PWRITEV, unix.SYS_FSTAT, unix.SYS_NEWFSTATAT, unix.SYS_STATX,
		unix.SYS_STAT, unix.SYS_LSTAT, unix.SYS_ACCESS, unix.SYS_FACCESSAT, unix.SYS_FACCESSAT2,
		unix.SYS_READLINK, unix.SYS_READLINKAT, unix.SYS_GETCWD, unix.SYS_GETDENTS64,
		unix.SYS_FCNTL, unix.SYS_DUP, unix.SYS_DUP2, unix.SYS_DUP3, unix.SYS_FADVISE64,
		unix.SYS_TRUNCATE, unix.SYS_FTRUNCATE, unix.SYS_FCHMOD, unix.SYS_FCHMODAT,
		unix.SYS_FCHOWN, unix.SYS_FCHOWNAT, unix.SYS_CHDIR, unix.SYS_FCHDIR, unix.SYS_UMASK,
		unix.SYS_GETPID, unix.SYS_GETTID, unix.SYS_GETPPID,
		unix.SYS_GETUID, unix.SYS_GETEUID, unix.SYS_GETGID, unix.SYS_GETEGID,
		unix.SYS_GETRESUID, unix.SYS_GETRESGID, unix.SYS_GETGROUPS,
		unix.SYS_PIPE, unix.SYS_PIPE2, unix.SYS_SELECT, unix.SYS_PSELECT6,
		unix.SYS_POLL, unix.SYS_PPOLL, unix.SYS_EPOLL_CREATE1, unix.SYS_EPOLL_CTL, unix.SYS_EPOLL_WAIT,
		unix.SYS_IOCTL, unix.SYS_FUTEX,
		unix.SYS_BRK, unix.SYS_MMAP, unix.SYS_MPROTECT, unix.SYS_MUNMAP, unix.SYS_MREMAP,
		unix.SYS_MADVISE, unix.SYS_MSYNC, unix.SYS_MINCORE,
		unix.SYS_RT_SIGACTION, unix.SYS_RT_SIGPROCMASK, unix.SYS_RT_SIGRETURN, unix.SYS_SIGALTSTACK,
		unix.SYS_CLONE, unix.SYS_CLONE3, unix.SYS_FORK, unix.SYS_VFORK, unix.SYS_EXIT, unix.SYS_EXIT_GROUP, unix.SYS_WAIT4,
		unix.SYS_SET_TID_ADDRESS, unix.SYS_SET_ROBUST_LIST, unix.SYS_RSEQ, unix.SYS_ARCH_PRCTL,
		unix.SYS_PRLIMIT64, unix.SYS_GETRLIMIT, unix.SYS_GETRUSAGE, unix.SYS_TIMES,
		unix.SYS_CLOCK_GETTIME, unix.SYS_CLOCK_GETRES, unix.SYS_CLOCK_NANOSLEEP, unix.SYS_NANOSLEEP,
		unix.SYS_SCHED_YIELD, unix.SYS_SCHED_GETAFFINITY, unix.SYS_UNAME, unix.SYS_SYSINFO,
		unix.SYS_GETRANDOM, unix.SYS_EXECVE, unix.SYS_STATFS, unix.SYS_FSTATFS,
	}
}

const (
	seccompOffsetNr   = 0
	seccompOffsetArch = 4
)

// buildSeccompFilter returns a classic BPF program that denies-by-default:
// any architecture other than the native one is killed, and any syscall not
// on the allowlist is killed. A match on an allowed syscall returns ALLOW.
func buildSeccompFilter(allowed []int) []unix.SockFilter {
	filter := make([]unix.SockFilter, 0, len(allowed)+6)
	filter = append(filter,
		bpfStmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, seccompOffsetArch),
		bpfJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, unix.AUDIT_ARCH_X86_64, 1, 0),
		bpfStmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_KILL_PROCESS),
		bpfStmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, seccompOffsetNr),
	)
	for i, nr := range allowed {
		// On match, skip the remaining comparisons and the default kill to
		// reach ALLOW; that is len(allowed)-i instructions below this one.
		filter = append(filter, bpfJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, uint32(nr), uint8(len(allowed)-i), 0))
	}
	filter = append(filter,
		bpfStmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_KILL_PROCESS),
		bpfStmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW),
	)
	return filter
}

func bpfStmt(code uint16, k uint32) unix.SockFilter {
	return unix.SockFilter{Code: code, K: k}
}

func bpfJump(code uint16, k uint32, jt, jf uint8) unix.SockFilter {
	return unix.SockFilter{Code: code, Jt: jt, Jf: jf, K: k}
}

// applySeccomp drops the child's privileges and installs the allowlist
// filter. It must run after rlimit and Landlock setup and immediately before
// execve so no further Go-runtime syscall is trapped by the filter.
func applySeccomp() error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return Errorf(ErrorCodeSandboxInitFailed, "set no_new_privs: %v", err)
	}
	filter := buildSeccompFilter(allowedSyscalls())
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, 0, uintptr(unsafe.Pointer(&prog))); errno != 0 {
		return Errorf(ErrorCodeSandboxInitFailed, "install seccomp filter: %v", errno)
	}
	return nil
}
