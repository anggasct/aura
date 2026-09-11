package discord

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument     ErrorCode = "invalid_argument"
	ErrorCodeConnectionFailed    ErrorCode = "connection_failed"
	ErrorCodeProtocolInvalid     ErrorCode = "protocol_invalid"
	ErrorCodeDeliveryUnavailable ErrorCode = "delivery_unavailable"
	ErrorCodeDeliveryAmbiguous   ErrorCode = "delivery_ambiguous"
	ErrorCodeDeliveryFailed      ErrorCode = "delivery_failed"
	ErrorCodeMessageTooLarge     ErrorCode = "message_too_large"
	ErrorCodeAttachmentRejected  ErrorCode = "attachment_rejected"
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
