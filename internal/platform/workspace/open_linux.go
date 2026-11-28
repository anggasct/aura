//go:build linux

package workspace

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"github.com/anggasct/aura/internal/platform/fault"
)

const resolveBeneath = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV

func OpenRoot(path string) (int, error) {
	return unix.Open(path, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
}

func OpenBeneath(root int, path string, flags, mode uint64) (int, error) {
	return unix.Openat2(root, path, &unix.OpenHow{Flags: flags, Mode: mode, Resolve: resolveBeneath})
}

func Escapes(err error) bool {
	return errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EXDEV) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EEXIST)
}

func OpenRead(workspace, relative string) (*os.File, error) {
	if err := ValidateRelative(relative); err != nil {
		return nil, err
	}
	root, err := OpenRoot(workspace)
	if err != nil {
		return nil, fmt.Errorf("open the workspace: %w", err)
	}
	defer func() { _ = unix.Close(root) }()
	fd, err := OpenBeneath(root, relative, uint64(unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK), 0)
	if err != nil {
		if Escapes(err) {
			return nil, fault.Wrap(ErrorCodeOutside, "path is outside the workspace", err)
		}
		return nil, fmt.Errorf("open a workspace file: %w", err)
	}
	return os.NewFile(uintptr(fd), "workspace-file"), nil
}
