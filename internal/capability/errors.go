package capability

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrorCodeProfileInvalid        ErrorCode = "profile_invalid"
	ErrorCodeCapabilityUnknown     ErrorCode = "capability_unknown"
	ErrorCodeCapabilityUnavailable ErrorCode = "capability_unavailable"
	ErrorCodeDependencyMissing     ErrorCode = "dependency_missing"
)

type Error struct {
	Code       ErrorCode
	Capability string
	Detail     string
}

func (e *Error) Error() string {
	if e.Capability == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Detail)
	}
	return fmt.Sprintf("%s: %s: %s", e.Code, e.Capability, e.Detail)
}

func CodeOf(err error) (ErrorCode, bool) {
	var target *Error
	if !errors.As(err, &target) {
		return "", false
	}
	return target.Code, true
}

func newError(code ErrorCode, capability, detail string) error {
	return &Error{Code: code, Capability: capability, Detail: detail}
}
