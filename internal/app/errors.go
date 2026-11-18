package app

type ErrorCode string

const (
	ErrorCodeInvalidArgument ErrorCode = "invalid_argument"
	ErrorCodeStopTimeout     ErrorCode = "stop_timeout"
)
