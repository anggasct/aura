//go:build windows

package restate

import (
	"os"
)

func terminatingSignal() os.Signal { return os.Interrupt }
