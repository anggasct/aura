package httpchat

type ErrorCode string

const (
	ErrorCodeInvalidArgument   ErrorCode = "invalid_argument"
	ErrorCodeKeyMalformed      ErrorCode = "key_malformed"
	ErrorCodeKeyGeneration     ErrorCode = "key_generation_failed"
	ErrorCodeSessionGeneration ErrorCode = "session_generation_failed"
	ErrorCodeConnectionFailed  ErrorCode = "connection_failed"
)
