//go:build !linux && !darwin

package terminal

import (
	"os"
)

func IsTerminal(fd int) bool {
	return false
}

func TerminalSize(fd int) (width, height int) {
	return 0, 0
}

func DetectTTY(in, out *os.File) bool {
	return false
}
