//go:build linux

package sandbox

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// landlockReadMask is the set of access rights a read-only path grants. It
// never includes write or create rights, so a tool handed a read-only root
// cannot mutate anything beneath it.
const landlockReadMask = unix.LANDLOCK_ACCESS_FS_EXECUTE |
	unix.LANDLOCK_ACCESS_FS_READ_FILE |
	unix.LANDLOCK_ACCESS_FS_READ_DIR

// landlockWriteMask is the read mask plus every write and create right the
// running ABI supports. REFER (v2) and TRUNCATE (v3) are added conditionally;
// requesting an unsupported right would make ruleset creation fail, so the
// mask is negotiated against the ABI rather than assumed.
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

// landlockABI returns the running kernel's Landlock ABI version, or 0 when
// the kernel does not expose one. The version drives which rights the
// ruleset may handle.
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

// applyLandlock builds a ruleset that confines the child to the read-only and
// read-write paths in spec and ties the child's threads to it. It runs in the
// sandbox child before exec; once enforced, any filesystem access outside the
// declared roots is denied at the kernel level.
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

// addLandlockRule grants the child access to everything beneath path within
// the access mask. The path is opened with O_PATH so the rule can be set
// without read or write permission on the path itself.
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
