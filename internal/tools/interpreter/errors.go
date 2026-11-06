package interpreter

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
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

type Error struct {
	Code   ErrorCode
	Detail string
	cause  error
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func (e *Error) Unwrap() error {
	return e.cause
}

func CodeOf(err error) (ErrorCode, bool) {
	var target *Error
	if !errors.As(err, &target) {
		return "", false
	}
	return target.Code, true
}

func Errorf(code ErrorCode, format string, args ...any) error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

func ioError(operation string, cause error) error {
	detail := operation + " failed"
	if reason := pathFreeReason(cause); reason != "" {
		detail += ": " + reason
	}
	return &Error{Code: ErrorCodeIO, Detail: detail, cause: cause}
}

func pathFreeReason(err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err.Error()
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return linkErr.Err.Error()
	}
	return ""
}
