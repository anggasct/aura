package interpreter

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/anggasct/aura/internal/platform/fault"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument     ErrorCode = "invalid_argument"
	ErrorCodeManifestInvalid     ErrorCode = "interpreter_manifest_invalid"
	ErrorCodeRuntimeUnknown      ErrorCode = "interpreter_runtime_unknown"
	ErrorCodeUnsupportedPlatform ErrorCode = "interpreter_unsupported_platform"
	ErrorCodeArchiveInvalid      ErrorCode = "interpreter_archive_invalid"
	ErrorCodeDigestMismatch      ErrorCode = "interpreter_digest_mismatch"
	ErrorCodeAlreadyInstalled    ErrorCode = "interpreter_already_installed"
	ErrorCodeInstallBusy         ErrorCode = "interpreter_install_busy"
	ErrorCodeIO                  ErrorCode = "interpreter_io_failed"
)

func Errorf(code ErrorCode, format string, args ...any) error {
	return fault.New(code, fmt.Sprintf(format, args...))
}

func ioError(operation string, cause error) error {
	detail := operation + " failed"
	if sanitized := pathFree(cause); sanitized != nil {
		return fault.Wrap(ErrorCodeIO, detail, sanitized)
	}
	return fault.New(ErrorCodeIO, detail)
}

func pathFree(err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return linkErr.Err
	}
	return err
}
