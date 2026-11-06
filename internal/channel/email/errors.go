package email

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument        ErrorCode = "invalid_argument"
	ErrorCodeCredentialsUnavailable ErrorCode = "credentials_unavailable"
	ErrorCodeConnectionFailed       ErrorCode = "connection_failed"
	ErrorCodeLoginRejected          ErrorCode = "login_rejected"
	ErrorCodeProtocolInvalid        ErrorCode = "protocol_invalid"
	ErrorCodeMessageUnavailable     ErrorCode = "message_unavailable"
	ErrorCodeIntakeFailed           ErrorCode = "intake_failed"
	ErrorCodeDeliveryAmbiguous      ErrorCode = "delivery_ambiguous"
	ErrorCodeDeliveryFailed         ErrorCode = "delivery_failed"
	ErrorCodeTLSRequired            ErrorCode = "tls_required"
	ErrorCodeTLSUntrusted           ErrorCode = "tls_untrusted"
	ErrorCodeSubmissionRejected     ErrorCode = "submission_rejected"
	ErrorCodeSubmissionDeferred     ErrorCode = "submission_deferred"
	ErrorCodeMessageInvalid         ErrorCode = "message_invalid"
	ErrorCodeHandoffUnavailable     ErrorCode = "handoff_unavailable"
	ErrorCodeAttachmentRejected     ErrorCode = "attachment_rejected"
)

type Error struct {
	Code   ErrorCode
	Detail string
	Cause  error
}

func (e *Error) Error() string {
	if e.Cause == nil {
		return fmt.Sprintf("%s: %s", e.Code, e.Detail)
	}
	return fmt.Sprintf("%s: %s: %v", e.Code, e.Detail, e.Cause)
}

func (e *Error) Unwrap() error {
	return e.Cause
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
