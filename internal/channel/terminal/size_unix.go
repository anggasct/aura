//go:build linux || darwin

package terminal

import (
	"os"

	"golang.org/x/sys/unix"
)

// TerminalSize returns the terminal's column and row count for fd. Width or
// height zero mean unknown; callers must treat unknown as degraded
// presentation only.
func TerminalSize(fd int) (width, height int) {
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0
	}
	return int(ws.Col), int(ws.Row)
}

// DetectTTY reports whether both files are terminals; the interactive
// presentation requires stdin and stdout to be terminals together.
func DetectTTY(in, out *os.File) bool {
	if in == nil || out == nil {
		return false
	}
	return IsTerminal(int(in.Fd())) && IsTerminal(int(out.Fd()))
}
