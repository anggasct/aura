package gmail

import "errors"

type Code string

const (
	CodeInvalidArgument  Code = "invalid_argument"
	CodeUnknownTool      Code = "unknown_tool"
	CodeSubjectMismatch  Code = "subject_mismatch"
	CodeReplyUnavailable Code = "reply_unavailable"
	CodeNotFound         Code = "not_found"
	CodeUnauthorized     Code = "unauthorized"
	CodeForbidden        Code = "forbidden"
	CodeRateLimited      Code = "rate_limited"
	CodeRequestRejected  Code = "request_rejected"
	CodeUpstream         Code = "upstream_error"
	CodeResponseInvalid  Code = "response_invalid"
	CodeMessageTooLarge  Code = "message_too_large"
	CodeRequestFailed    Code = "request_failed"
	CodeEgressDenied     Code = "egress_denied"
)

type Error struct {
	Code Code
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return "gmail connector: " + string(e.Code)
}

func (e *Error) ErrorCode() string {
	if e == nil {
		return string(CodeUpstream)
	}
	return string(e.Code)
}

func fail(code Code) error {
	return &Error{Code: code}
}

func codeOf(err error) (Code, bool) {
	var target *Error
	if errors.As(err, &target) && target != nil {
		return target.Code, true
	}
	return "", false
}
