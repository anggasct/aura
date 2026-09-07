//go:build linux

package terminal

import (
	"golang.org/x/sys/unix"
)

func IsTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	return err == nil
}
