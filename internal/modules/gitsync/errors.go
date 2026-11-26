package gitsync

import (
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument   ErrorCode = "invalid_argument"
	ErrorCodeGateRefused       ErrorCode = "sync_unavailable"
	ErrorCodeNotConfigured     ErrorCode = "sync_not_configured"
	ErrorCodeNoUnknownState    ErrorCode = "sync_no_unknown_state"
	ErrorCodeManifestInvalid   ErrorCode = "sync_manifest_invalid"
	ErrorCodeTransportUnsafe   ErrorCode = "sync_transport_unsafe"
	ErrorCodeCredentialInvalid ErrorCode = "sync_secret_invalid"
	ErrorCodeEgressDenied      ErrorCode = "sync_egress_denied"
	ErrorCodePathDenied        ErrorCode = "sync_path_denied"
	ErrorCodeConflict          ErrorCode = "sync_conflict"
)

func Errorf(code ErrorCode, format string, args ...any) error {
	return fault.New(code, fmt.Sprintf(format, args...))
}

func errNilArgument(name string) error {
	return fault.New(ErrorCodeInvalidArgument, name+" must not be nil")
}
