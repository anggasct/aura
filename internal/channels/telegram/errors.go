package telegram

import (
	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument     ErrorCode = "invalid_argument"
	ErrorCodeConnectionFailed    ErrorCode = "connection_failed"
	ErrorCodeProtocolInvalid     ErrorCode = "protocol_invalid"
	ErrorCodeBotAPIRejected      ErrorCode = "bot_api_rejected"
	ErrorCodeIntakeFailed        ErrorCode = "intake_failed"
	ErrorCodeDeliveryUnavailable ErrorCode = "delivery_unavailable"
	ErrorCodeDeliveryAmbiguous   ErrorCode = "delivery_ambiguous"
	ErrorCodeDeliveryFailed      ErrorCode = "delivery_failed"
	ErrorCodeApprovalUnavailable ErrorCode = "approval_unavailable"
	ErrorCodeApprovalTimeout     ErrorCode = "approval_timeout"
	ErrorCodeHandoffUnavailable  ErrorCode = "handoff_unavailable"
	ErrorCodeAttachmentRejected  ErrorCode = "attachment_rejected"
)

func deliveryAmbiguous(detail string) error {
	return fault.Wrap(ErrorCodeDeliveryAmbiguous, detail, runtimechannelhost.ErrDeliveryAmbiguous)
}

func approvalUnavailable(detail string) error {
	return fault.Wrap(ErrorCodeApprovalUnavailable, detail, approval.ErrUnavailable)
}

func approvalTimeout(detail string) error {
	return fault.Wrap(ErrorCodeApprovalTimeout, detail, approval.ErrTimeout)
}
