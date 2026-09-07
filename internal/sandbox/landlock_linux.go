//go:build linux

package sandbox

import (
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

const landlockReadMask = unix.LANDLOCK_ACCESS_FS_EXECUTE |
	unix.LANDLOCK_ACCESS_FS_READ_FILE |
	unix.LANDLOCK_ACCESS_FS_READ_DIR

func landlockWriteMask(abiVersion int) uint64 {
	m := landlockReadMask |
		unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
		unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK
	if abiVersion >= 2 {
		m |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abiVersion >= 3 {
		m |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	return uint64(m)
}

func landlockRuntimeRoots() []string {
	candidates := []string{"/usr", "/lib", "/lib64", "/bin", "/sbin"}
	var roots []string
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			roots = append(roots, c)
		}
	}
	return roots
}

func landlockABI() (int, error) {
	v, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return 0, errno
	}
	return int(v), nil
}

type landlockRulesetAttr struct {
	handledAccessFS uint64
}

type landlockPathBeneathAttr struct {
	allowedAccess uint64
	parentFd      int32
}

func applyLandlock(spec *Spec) error {
	abi, err := landlockABI()
	if err != nil || abi <= 0 {
		return Errorf(ErrorCodeSandboxInitFailed, "landlock abi unavailable: %v", err)
	}
	writeMask := landlockWriteMask(abi)
	attr := landlockRulesetAttr{handledAccessFS: writeMask}
	ruleset, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return Errorf(ErrorCodeSandboxInitFailed, "landlock create_ruleset: %v", errno)
	}
	defer func() { _ = unix.Close(int(ruleset)) }()

	for _, root := range landlockRuntimeRoots() {
		if err := addLandlockRule(int(ruleset), root, landlockReadMask); err != nil {
			return err
		}
	}
	for _, path := range spec.ReadOnlyPaths {
		if err := addLandlockRule(int(ruleset), path, landlockReadMask); err != nil {
			return err
		}
	}
	for _, path := range append([]string{spec.WorkingDir}, spec.ReadWritePaths...) {
		if err := addLandlockRule(int(ruleset), path, writeMask); err != nil {
			return err
		}
	}

	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, ruleset, 0, 0); errno != 0 {
		return Errorf(ErrorCodeSandboxInitFailed, "landlock restrict_self: %v", errno)
	}
	return nil
}

func addLandlockRule(ruleset int, path string, access uint64) error {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return Errorf(ErrorCodeSandboxInitFailed, "open %s for landlock: %v", path, err)
	}
	defer func() { _ = unix.Close(fd) }()
	pb := landlockPathBeneathAttr{allowedAccess: access, parentFd: int32(fd)}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_ADD_RULE, uintptr(ruleset), unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&pb))); errno != 0 {
		return Errorf(ErrorCodeSandboxInitFailed, "landlock add_rule %s: %v", path, errno)
	}
	return nil
}
