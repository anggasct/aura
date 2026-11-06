//go:build unix

package knowledge

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func openNoFollow(path string) (*os.File, error) {
	name := filepath.Base(path)
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), name), nil
}

func isSymlinkOpenError(err error) bool {
	return errors.Is(err, syscall.ELOOP)
}
