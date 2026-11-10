//go:build linux || darwin

package terminal

import (
	"os"

	"golang.org/x/sys/unix"
)

func TerminalSize(fd int) (width, height int) {
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0
	}
	return int(ws.Col), int(ws.Row)
}

func DetectTTY(in, out *os.File) bool {
	if in == nil || out == nil {
		return false
	}
	return IsTerminal(int(in.Fd())) && IsTerminal(int(out.Fd()))
}
