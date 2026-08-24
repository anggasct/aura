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

// IsHealthState reports whether every problem in err describes an
// artifact or host capability state (absent from this build, wrong
// profile or OS, missing dependency) rather than a malformed
// configuration. Health-state problems are reported through diagnostics
// surfaces; malformed configurations remain load errors.
func IsHealthState(err error) bool {
	if err == nil {
		return false
	}
	var typed *Error
	if errors.As(err, &typed) {
		return healthStateCode(typed.Code)
	}
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		parts := joined.Unwrap()
		if len(parts) == 0 {
			return false
		}
		for _, part := range parts {
			if !IsHealthState(part) {
				return false
			}
		}
		return true
	}
	return false
}

func healthStateCode(code ErrorCode) bool {
	return code == ErrorCodeCapabilityUnavailable || code == ErrorCodeDependencyMissing
}
