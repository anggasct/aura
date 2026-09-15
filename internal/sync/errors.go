package sync

import (
	"errors"
	"fmt"
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

type Error struct {
	Code   ErrorCode
	Detail string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
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

func errNilArgument(name string) error {
	return &Error{Code: ErrorCodeInvalidArgument, Detail: name + " must not be nil"}
}
