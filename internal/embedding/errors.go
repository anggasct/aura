package embedding

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrorCodeConfigInvalid     ErrorCode = "embedding_config_invalid"
	ErrorCodeRequestInvalid    ErrorCode = "embedding_request_invalid"
	ErrorCodeRequestFailed     ErrorCode = "embedding_request_failed"
	ErrorCodeStatusRejected    ErrorCode = "embedding_status_rejected"
	ErrorCodeResponseInvalid   ErrorCode = "embedding_response_invalid"
	ErrorCodeRedirectRefused   ErrorCode = "embedding_redirect_refused"
	ErrorCodeDestinationDenied ErrorCode = "embedding_destination_denied"
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
