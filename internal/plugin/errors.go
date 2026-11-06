package plugin

import (
	"errors"
	"fmt"

	"github.com/anggasct/aura/internal/plugin/auth"
	"github.com/anggasct/aura/internal/plugin/connector"
)

type ErrorCode string

const (
	ErrManifestInvalid    ErrorCode = "plugin_manifest_invalid"
	ErrManifestTooLarge   ErrorCode = "plugin_manifest_too_large"
	ErrBackendUnsupported ErrorCode = "plugin_backend_unsupported"
	ErrPathInvalid        ErrorCode = "plugin_path_invalid"
	ErrBundleTooLarge     ErrorCode = "plugin_bundle_too_large"
	ErrVersionConflict    ErrorCode = "plugin_version_conflict"
	ErrDigestMismatch     ErrorCode = "plugin_digest_mismatch"
	ErrNotFound           ErrorCode = "plugin_not_found"
	ErrTampered           ErrorCode = "plugin_tampered"
	ErrToolDrift          ErrorCode = ErrorCode(connector.ErrToolDrift)
	ErrSurfacePending     ErrorCode = "plugin_surface_pending"
	ErrDrift              ErrorCode = "plugin_drift"
	ErrInvalidArgument    ErrorCode = "plugin_invalid_argument"
	ErrStorage            ErrorCode = "plugin_storage_unavailable"
	ErrBackendUnavailable ErrorCode = ErrorCode(connector.ErrBackendUnavailable)
	ErrAuthRequired       ErrorCode = ErrorCode(auth.ErrRequired)
	ErrAuthFailed         ErrorCode = ErrorCode(auth.ErrFailed)
)

type Error struct {
	Code   ErrorCode
	Detail string
	Err    error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Detail == "" {
		return string(e.Code)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func CodeOf(err error) (ErrorCode, bool) {
	var target *Error
	if errors.As(err, &target) && target != nil {
		return target.Code, true
	}
	return "", false
}

func Errorf(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

func Wrap(code ErrorCode, err error, detail string) *Error {
	return &Error{Code: code, Detail: detail, Err: err}
}
