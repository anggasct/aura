//go:build linux || darwin

package cli

import (
	"errors"
	"math"
	"syscall"

	"golang.org/x/sys/unix"
)

// diskFreeBytes reports free space available to unprivileged writes for the
// filesystem holding path. The product goes through float64 because the
// kernel block-count types differ per platform; anything above the int64
// range clamps to MaxInt64, far beyond any real filesystem.
func diskFreeBytes(path string) (int64, error) {
	var stats unix.Statfs_t
	if err := unix.Statfs(path, &stats); err != nil {
		return 0, err
	}
	if stats.Bavail <= 0 || stats.Bsize <= 0 {
		return 0, nil
	}
	free := float64(stats.Bavail) * float64(stats.Bsize)
	if free >= math.MaxInt64 {
		return math.MaxInt64, nil
	}
	return int64(free), nil
}

// writableProbe reports whether the process could write to path without
// writing anything.
func writableProbe(path string) error {
	return unix.Access(path, unix.W_OK)
}

// classifyOpenError names the syscall class of a failed writable probe: a
// read-only mount reports EROFS.
func classifyOpenError(err error) bool {
	return errors.Is(err, syscall.EROFS)
}
