//go:build linux && amd64

package sandbox

import (
	"math"
	"slices"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func seccompAvailable() bool {
	action := unix.SECCOMP_RET_ALLOW
	_, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_GET_ACTION_AVAIL, 0, uintptr(unsafe.Pointer(&action)))
	return errno != syscall.ENOSYS
}

const (
	seccompOffsetNr       = 0
	seccompOffsetArch     = 4
	seccompOffsetArgs     = 16
	seccompArgStride      = 8
	seccompMaxJump        = math.MaxUint8
	seccompNamespaceFlags = unix.CLONE_NEWNS | unix.CLONE_NEWCGROUP | unix.CLONE_NEWUTS | unix.CLONE_NEWIPC |
		unix.CLONE_NEWUSER | unix.CLONE_NEWPID | unix.CLONE_NEWNET | unix.CLONE_NEWTIME
)

type seccompCheckKind int

const (
	checkArgEquals seccompCheckKind = iota
	checkArgNotIn
	checkMaskClear
	checkMaskSet
)

type seccompCheck struct {
	kind   seccompCheckKind
	arg    int
	values []uint32
}

func argEquals(arg int, value uint32) seccompCheck {
	return seccompCheck{kind: checkArgEquals, arg: arg, values: []uint32{value}}
}

func argNotIn(arg int, values ...uint32) seccompCheck {
	return seccompCheck{kind: checkArgNotIn, arg: arg, values: values}
}

func maskClear(arg int, mask uint32) seccompCheck {
	return seccompCheck{kind: checkMaskClear, arg: arg, values: []uint32{mask}}
}

func maskSet(arg int, mask uint32) seccompCheck {
	return seccompCheck{kind: checkMaskSet, arg: arg, values: []uint32{mask}}
}

type seccompRule struct {
	nr     int
	errno  uint32
	checks []seccompCheck
}

func allowWhen(nr int, checks ...seccompCheck) seccompRule {
	return seccompRule{nr: nr, checks: checks}
}

func failWith(nr int, errno syscall.Errno) seccompRule {
	return seccompRule{nr: nr, errno: uint32(errno)}
}

type seccompProfile struct {
	allow []int
	rules []seccompRule
}

func baseSyscalls() []int {
	return []int{
		unix.SYS_READ, unix.SYS_WRITE, unix.SYS_OPENAT, unix.SYS_CLOSE, unix.SYS_CLOSE_RANGE,
		unix.SYS_LSEEK, unix.SYS_PREAD64, unix.SYS_PWRITE64, unix.SYS_READV, unix.SYS_WRITEV,
		unix.SYS_PREADV, unix.SYS_PWRITEV, unix.SYS_FSTAT, unix.SYS_NEWFSTATAT, unix.SYS_STATX,
		unix.SYS_STAT, unix.SYS_LSTAT, unix.SYS_ACCESS, unix.SYS_FACCESSAT, unix.SYS_FACCESSAT2,
		unix.SYS_READLINK, unix.SYS_READLINKAT, unix.SYS_GETCWD, unix.SYS_GETDENTS64,
		unix.SYS_DUP, unix.SYS_DUP2, unix.SYS_DUP3, unix.SYS_FADVISE64,
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
		unix.SYS_FORK, unix.SYS_VFORK, unix.SYS_EXIT, unix.SYS_EXIT_GROUP, unix.SYS_WAIT4,
		unix.SYS_SET_TID_ADDRESS, unix.SYS_SET_ROBUST_LIST, unix.SYS_RSEQ, unix.SYS_ARCH_PRCTL,
		unix.SYS_GETRLIMIT, unix.SYS_GETRUSAGE, unix.SYS_TIMES,
		unix.SYS_CLOCK_GETTIME, unix.SYS_CLOCK_GETRES, unix.SYS_CLOCK_NANOSLEEP, unix.SYS_NANOSLEEP,
		unix.SYS_SCHED_YIELD, unix.SYS_SCHED_GETAFFINITY, unix.SYS_UNAME, unix.SYS_SYSINFO,
		unix.SYS_GETRANDOM, unix.SYS_EXECVE, unix.SYS_STATFS, unix.SYS_FSTATFS,
	}
}

func sharedRules() []seccompRule {
	return []seccompRule{
		allowWhen(unix.SYS_PRLIMIT64, argEquals(0, 0)),
		allowWhen(unix.SYS_FCNTL, argNotIn(1, unix.F_SETOWN, unix.F_SETOWN_EX, unix.F_SETSIG)),
		failWith(unix.SYS_CLONE3, syscall.ENOSYS),
	}
}

func metadataSyscalls() []int {
	return []int{
		unix.SYS_CHMOD, unix.SYS_FCHMOD, unix.SYS_FCHMODAT, unix.SYS_FCHMODAT2,
		unix.SYS_CHOWN, unix.SYS_FCHOWN, unix.SYS_LCHOWN, unix.SYS_FCHOWNAT,
		unix.SYS_UTIME, unix.SYS_UTIMES, unix.SYS_FUTIMESAT, unix.SYS_UTIMENSAT,
	}
}

func interpreterAdditions() []int {
	return []int{
		unix.SYS_CAPGET, unix.SYS_EPOLL_PWAIT, unix.SYS_EVENTFD2, unix.SYS_GETSOCKNAME, unix.SYS_GETPEERNAME, unix.SYS_GETSOCKOPT,
		unix.SYS_SCHED_GETSCHEDULER, unix.SYS_SCHED_GETPARAM,
		unix.SYS_SENDTO, unix.SYS_RECVFROM,
		unix.SYS_MKDIR, unix.SYS_RMDIR, unix.SYS_UNLINK, unix.SYS_UNLINKAT,
		unix.SYS_RENAME, unix.SYS_RENAMEAT, unix.SYS_RENAMEAT2,
		unix.SYS_FSYNC, unix.SYS_SENDFILE, unix.SYS_TGKILL,
	}
}

func failAll(errno syscall.Errno, calls ...int) []seccompRule {
	rules := make([]seccompRule, 0, len(calls))
	for _, nr := range calls {
		rules = append(rules, failWith(nr, errno))
	}
	return rules
}

func interpreterProfile() seccompProfile {
	allow := slices.DeleteFunc(baseSyscalls(), func(nr int) bool { return slices.Contains(metadataSyscalls(), nr) })
	allow = append(allow, interpreterAdditions()...)
	rules := append(sharedRules(),
		allowWhen(unix.SYS_CLONE, maskClear(0, seccompNamespaceFlags)),
		failWith(unix.SYS_PRCTL, syscall.EPERM),
		allowWhen(unix.SYS_SOCKETPAIR, argEquals(0, unix.AF_UNIX)),
	)
	rules = append(rules, failAll(syscall.EACCES, unix.SYS_SOCKET, unix.SYS_CONNECT, unix.SYS_BIND)...)
	rules = append(rules, failAll(syscall.ENOSYS, unix.SYS_IO_URING_SETUP, unix.SYS_PIDFD_OPEN, unix.SYS_PIDFD_SEND_SIGNAL)...)
	rules = append(rules, failAll(syscall.EPERM, metadataSyscalls()...)...)
	return seccompProfile{allow: allow, rules: rules}
}

func buildProfile(profile Profile, pid uint32) (seccompProfile, error) {
	switch profile {
	case ProfileBase:
		rules := append(sharedRules(),
			allowWhen(unix.SYS_CLONE, maskClear(0, seccompNamespaceFlags)),
			failWith(unix.SYS_PRCTL, syscall.EPERM),
			failWith(unix.SYS_TGKILL, syscall.EPERM),
		)
		return seccompProfile{allow: baseSyscalls(), rules: rules}, nil
	case ProfileConnector:
		allow := slices.DeleteFunc(baseSyscalls(), func(nr int) bool { return nr == unix.SYS_FORK || nr == unix.SYS_VFORK })
		allow = append(allow, unix.SYS_EPOLL_PWAIT, unix.SYS_EVENTFD2)
		rules := append(sharedRules(),
			allowWhen(unix.SYS_CLONE, maskClear(0, seccompNamespaceFlags), maskSet(0, unix.CLONE_THREAD)),
			failWith(unix.SYS_PRCTL, syscall.EPERM),
			allowWhen(unix.SYS_TGKILL, argEquals(0, pid)),
		)
		return seccompProfile{allow: allow, rules: rules}, nil
	case ProfileInterpreter:
		return interpreterProfile(), nil
	default:
		return seccompProfile{}, Errorf(ErrorCodeInvalidArgument, "unknown sandbox profile")
	}
}

func buildSeccompFilter(profile *seccompProfile) ([]unix.SockFilter, error) {
	seen := map[int]struct{}{}
	for _, nr := range profile.allow {
		if _, dup := seen[nr]; dup {
			return nil, Errorf(ErrorCodeSandboxInitFailed, "seccomp profile lists syscall %d twice", nr)
		}
		seen[nr] = struct{}{}
	}
	for i := range profile.rules {
		nr := profile.rules[i].nr
		if _, dup := seen[nr]; dup {
			return nil, Errorf(ErrorCodeSandboxInitFailed, "seccomp profile lists syscall %d twice", nr)
		}
		seen[nr] = struct{}{}
	}
	if len(profile.allow) > seccompMaxJump {
		return nil, Errorf(ErrorCodeSandboxInitFailed, "seccomp allowlist exceeds the jump range")
	}
	filter := make([]unix.SockFilter, 0, len(profile.allow)+8*len(profile.rules)+8)
	filter = append(filter,
		bpfStmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, seccompOffsetArch),
		bpfJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, unix.AUDIT_ARCH_X86_64, 1, 0),
		bpfStmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_KILL_PROCESS),
		bpfStmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, seccompOffsetNr),
	)
	for i := range profile.rules {
		block, err := ruleBlock(&profile.rules[i])
		if err != nil {
			return nil, err
		}
		filter = append(filter, block...)
	}
	for i, nr := range profile.allow {
		filter = append(filter, bpfJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, uint32(nr), uint8(len(profile.allow)-i), 0))
	}
	filter = append(filter,
		bpfStmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_KILL_PROCESS),
		bpfStmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW),
	)
	return filter, nil
}

func ruleBlock(rule *seccompRule) ([]unix.SockFilter, error) {
	if rule.errno != 0 {
		return errnoBlock(rule), nil
	}
	return checkedBlock(rule)
}

func errnoBlock(rule *seccompRule) []unix.SockFilter {
	return []unix.SockFilter{
		bpfJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, uint32(rule.nr), 0, 1),
		bpfStmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ERRNO|(rule.errno&unix.SECCOMP_RET_DATA)),
	}
}

func checkedBlock(rule *seccompRule) ([]unix.SockFilter, error) {
	size := 2
	for _, check := range rule.checks {
		size += check.length()
	}
	if size > seccompMaxJump {
		return nil, Errorf(ErrorCodeSandboxInitFailed, "seccomp rule exceeds the jump range")
	}
	killAt := size - 1
	block := make([]unix.SockFilter, 0, size+1)
	block = append(block, bpfJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, uint32(rule.nr), 0, uint8(size)))
	for _, check := range rule.checks {
		block = append(block, check.instructions(len(block)-1, killAt)...)
	}
	block = append(block,
		bpfStmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW),
		bpfStmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_KILL_PROCESS),
	)
	return block, nil
}

func (c seccompCheck) length() int {
	if c.kind == checkArgNotIn {
		return 1 + len(c.values)
	}
	return 2
}

func (c seccompCheck) instructions(base, killAt int) []unix.SockFilter {
	toKill := func(position int) uint8 { return uint8(killAt - (base + position) - 1) }
	load := bpfStmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, uint32(seccompOffsetArgs+seccompArgStride*c.arg))
	jeq := uint16(unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K)
	jset := uint16(unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K)
	switch c.kind {
	case checkArgEquals:
		return []unix.SockFilter{load, bpfJump(jeq, c.values[0], 0, toKill(1))}
	case checkArgNotIn:
		out := []unix.SockFilter{load}
		for i, value := range c.values {
			out = append(out, bpfJump(jeq, value, toKill(1+i), 0))
		}
		return out
	case checkMaskClear:
		return []unix.SockFilter{load, bpfJump(jset, c.values[0], toKill(1), 0)}
	case checkMaskSet:
		return []unix.SockFilter{load, bpfJump(jset, c.values[0], 0, toKill(1))}
	default:
		return []unix.SockFilter{load, bpfStmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_KILL_PROCESS)}
	}
}

func bpfStmt(code uint16, k uint32) unix.SockFilter {
	return unix.SockFilter{Code: code, K: k}
}

func bpfJump(code uint16, k uint32, jt, jf uint8) unix.SockFilter {
	return unix.SockFilter{Code: code, Jt: jt, Jf: jf, K: k}
}

func applySeccomp(profile Profile) error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return Errorf(ErrorCodeSandboxInitFailed, "set no_new_privs: %v", err)
	}
	resolved, err := buildProfile(profile, uint32(unix.Getpid()))
	if err != nil {
		return err
	}
	filter, err := buildSeccompFilter(&resolved)
	if err != nil {
		return err
	}
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, 0, uintptr(unsafe.Pointer(&prog))); errno != 0 {
		return Errorf(ErrorCodeSandboxInitFailed, "install seccomp filter: %v", errno)
	}
	return nil
}
