//go:build linux || darwin

package restate

import (
	"os"
	"syscall"
)

func terminatingSignal() os.Signal { return syscall.SIGTERM }
