package plugin

import (
	"fmt"

	"github.com/anggasct/aura/internal/modules/plugin/auth"
	"github.com/anggasct/aura/internal/modules/plugin/connector"
	"github.com/anggasct/aura/internal/platform/fault"
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

func Errorf(code ErrorCode, format string, args ...any) error {
	return fault.New(code, fmt.Sprintf(format, args...))
}
