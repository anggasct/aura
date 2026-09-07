//go:build darwin

package terminal

import (
	"golang.org/x/sys/unix"
)

func IsTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	return err == nil
}
