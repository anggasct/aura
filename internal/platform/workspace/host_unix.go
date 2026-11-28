//go:build unix

package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/anggasct/aura/internal/platform/fault"
)

func OpenHost(path string) (*os.File, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve a host path: %w", err)
	}
	fd, err := unix.Open(absolute, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fault.Wrap(ErrorCodeSymlink, "path is a symbolic link", err)
		}
		return nil, fmt.Errorf("open a host file: %w", err)
	}
	return os.NewFile(uintptr(fd), filepath.Base(absolute)), nil
}
