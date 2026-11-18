package capability

import (
	"errors"
	"slices"

	"github.com/anggasct/aura/internal/platform/fault"
)

type ErrorCode string

const (
	ErrorCodeProfileInvalid        ErrorCode = "profile_invalid"
	ErrorCodeCapabilityUnknown     ErrorCode = "capability_unknown"
	ErrorCodeCapabilityUnavailable ErrorCode = "capability_unavailable"
	ErrorCodeDependencyMissing     ErrorCode = "dependency_missing"
)

func newError(code ErrorCode, capability, detail string) error {
	return fault.New(code, subject(capability, detail))
}

func subject(capability, detail string) string {
	if capability == "" {
		return detail
	}
	return capability + ": " + detail
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
	var typed *fault.Error[ErrorCode]
	if errors.As(err, &typed) {
		return slices.Contains(codes, typed.Code)
	}
	return false
}
