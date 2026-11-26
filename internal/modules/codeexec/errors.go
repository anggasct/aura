package codeexec

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/anggasct/aura/internal/platform/fault"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument ErrorCode = "invalid_argument"
	ErrorCodeUnavailable     ErrorCode = "runtime_unavailable"
	ErrorCodeRunFailed       ErrorCode = "run_failed"
	ErrorCodeArtifactQuota   ErrorCode = "artifact_quota"
)

func Errorf(code ErrorCode, format string, args ...any) error {
	return fault.New(code, fmt.Sprintf(format, args...))
}

func wrapf(code ErrorCode, cause error, format string, args ...any) error {
	return fault.Wrap(code, fmt.Sprintf(format, args...), cause)
}

func pathFree(err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}
