package capability

import (
	"errors"
	"fmt"
	"slices"
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

func IsHealthState(err error) bool {
	return onlyCodes(err, ErrorCodeCapabilityUnavailable, ErrorCodeDependencyMissing)
}

func IsMissingDependency(err error) bool {
	return onlyCodes(err, ErrorCodeDependencyMissing)
}

func onlyCodes(err error, codes ...ErrorCode) bool {
	if err == nil {
		return false
	}
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		parts := joined.Unwrap()
		if len(parts) == 0 {
			return false
		}
		for _, part := range parts {
			if !onlyCodes(part, codes...) {
				return false
			}
		}
		return true
	}
	var typed *Error
	if errors.As(err, &typed) {
		return slices.Contains(codes, typed.Code)
	}
	return false
}
