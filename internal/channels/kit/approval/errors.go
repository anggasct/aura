package approval

import "errors"

type ErrorCode string

const (
	ErrorCodeInvalidArgument    ErrorCode = "invalid_argument"
	ErrorCodeEntropyUnavailable ErrorCode = "entropy_unavailable"
)

var (
	ErrUnavailable = errors.New("approval channel unavailable")
	ErrTimeout     = errors.New("approval timed out")
)
