//go:build linux

package terminal

import (
	"golang.org/x/sys/unix"
)

// IsTerminal reports whether fd refers to a terminal. A failed probe fails
// closed: presentation degrades to the plain contract.
func IsTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	return err == nil
}
