//go:build windows

package cli

import (
	"errors"
	"os"
)

var errDiskSpaceUnsupported = errors.New("disk space check unsupported on this platform")

func diskFreeBytes(string) (int64, error) {
	return 0, errDiskSpaceUnsupported
}

// writableProbe has no portable access check; the write-path probe reports
// unknown writability rather than a false positive.
func writableProbe(path string) error {
	handle, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	return handle.Close()
}

func classifyOpenError(error) bool { return false }
