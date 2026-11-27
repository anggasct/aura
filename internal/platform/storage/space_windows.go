//go:build windows

package storage

import (
	"errors"
	"os"
)

var errDiskSpaceUnsupported = errors.New("disk space check unsupported on this platform")

func DiskFreeBytes(string) (int64, error) {
	return 0, errDiskSpaceUnsupported
}

func WritableProbe(path string) error {
	handle, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	return handle.Close()
}

func ClassifyOpenError(error) bool { return false }

func DiskUsage(string) (int64, int64, int64, error) {
	return 0, 0, 0, errDiskSpaceUnsupported
}
