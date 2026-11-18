package server

type ErrorCode string

const (
	ErrorCodeInvalidArgument  ErrorCode = "invalid_argument"
	ErrorCodeListenerPanicked ErrorCode = "listener_panicked"
	ErrorCodeStopTimeout      ErrorCode = "stop_timeout"
)
