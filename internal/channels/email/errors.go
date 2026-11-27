package email

import (
	"errors"

	"github.com/anggasct/aura/internal/platform/fault"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
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

func deliveryAmbiguous(detail string, cause error) error {
	if cause != nil {
		return fault.Wrap(ErrorCodeDeliveryAmbiguous, detail, errors.Join(runtimechannelhost.ErrDeliveryAmbiguous, cause))
	}
	return fault.Wrap(ErrorCodeDeliveryAmbiguous, detail, runtimechannelhost.ErrDeliveryAmbiguous)
}
