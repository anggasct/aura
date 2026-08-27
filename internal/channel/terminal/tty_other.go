//go:build !linux && !darwin

package terminal

import (
	"os"
)

// IsTerminal always reports false on platforms without terminal probes: the
// console fails closed to the plain presentation contract.
func IsTerminal(fd int) bool {
	return false
}

// TerminalSize reports unknown dimensions on platforms without probes.
func TerminalSize(fd int) (width, height int) {
	return 0, 0
}

// DetectTTY always reports false on platforms without terminal probes.
func DetectTTY(in, out *os.File) bool {
	return false
}
