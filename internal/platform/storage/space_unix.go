//go:build linux || darwin

package storage

import (
	"errors"
	"math"
	"syscall"

	"golang.org/x/sys/unix"
)

func DiskFreeBytes(path string) (int64, error) {
	free, _, _, err := DiskUsage(path)
	return free, err
}

func DiskUsage(path string) (freeBytes, totalBytes, freeInodes int64, err error) {
	var stats unix.Statfs_t
	if err := unix.Statfs(path, &stats); err != nil {
		return 0, 0, 0, err
	}
	if stats.Bavail <= 0 || stats.Bsize <= 0 {
		return 0, 0, int64(math.Min(float64(stats.Ffree), math.MaxInt64)), nil
	}
	free := float64(stats.Bavail) * float64(stats.Bsize)
	total := float64(stats.Blocks) * float64(stats.Bsize)
	if free >= math.MaxInt64 {
		free = math.MaxInt64
	}
	if total >= math.MaxInt64 {
		total = math.MaxInt64
	}
	freeInodes = int64(math.Min(float64(stats.Ffree), math.MaxInt64))
	return int64(free), int64(total), freeInodes, nil
}

func WritableProbe(path string) error {
	return unix.Access(path, unix.W_OK)
}

func ClassifyOpenError(err error) bool {
	return errors.Is(err, syscall.EROFS)
}
