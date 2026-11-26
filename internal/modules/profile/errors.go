package profile

type ErrorCode string

const (
	ErrorCodeInvalidArgument    ErrorCode = "invalid_argument"
	ErrorCodeProfileNotFound    ErrorCode = "profile_not_found"
	ErrorCodeProfileConflict    ErrorCode = "profile_conflict"
	ErrorCodeProfileInvalid     ErrorCode = "profile_invalid"
	ErrorCodeProfileUnavailable ErrorCode = "profile_unavailable"
)
