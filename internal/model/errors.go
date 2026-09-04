package model

import (
	"errors"
	"fmt"
	"strings"
)

type ErrorCode string

const (
	ErrorCodeProtocolInvalid       ErrorCode = "model_protocol_invalid"
	ErrorCodeCapabilityUnsupported ErrorCode = "model_capability_unsupported"
	ErrorCodeSecretInvalid         ErrorCode = "model_secret_invalid"
	ErrorCodeAuthFailed            ErrorCode = "model_auth_failed"
	ErrorCodeNotFound              ErrorCode = "model_not_found"
	ErrorCodeRateLimited           ErrorCode = "model_rate_limited"
	ErrorCodeOverloaded            ErrorCode = "model_overloaded"
	ErrorCodeContextTooLong        ErrorCode = "model_context_too_long"
	ErrorCodeContentFiltered       ErrorCode = "model_content_filtered"
	ErrorCodeStreamInvalid         ErrorCode = "model_stream_invalid"
	ErrorCodeConnectionFailed      ErrorCode = "model_connection_failed"
	ErrorCodeRouteInvalid          ErrorCode = "model_route_invalid"
	ErrorCodeBudgetExceeded        ErrorCode = "model_budget_exceeded"
	ErrorCodeDeadlineExceeded      ErrorCode = "model_deadline_exceeded"
)

type Error struct {
	Code       ErrorCode
	Model      string
	Capability string
	Detail     string
}

func (e *Error) Error() string {
	parts := []string{string(e.Code)}
	if e.Model != "" {
		parts = append(parts, e.Model)
	}
	if e.Capability != "" {
		parts = append(parts, e.Capability)
	}
	return fmt.Sprintf("%s: %s", strings.Join(parts, ": "), e.Detail)
}

func CodeOf(err error) (ErrorCode, bool) {
	var target *Error
	if !errors.As(err, &target) {
		return "", false
	}
	return target.Code, true
}

var (
	ErrRateLimited      = errors.New("model: rate limited")
	ErrAuthFailed       = errors.New("model: authentication failed")
	ErrModelNotFound    = errors.New("model: model not found")
	ErrOverloaded       = errors.New("model: provider overloaded")
	ErrContentFiltered  = errors.New("model: content filtered")
	ErrContextTooLong   = errors.New("model: context too long")
	ErrInvalidToolCall  = errors.New("model: invalid tool call")
	ErrConnectionFailed = errors.New("model: connection failed")
	ErrStreamIdle       = errors.New("model: stream idle timeout")
	ErrStreamTruncated  = errors.New("model: stream ended before the terminal event")
)

func newError(code ErrorCode, model, capability, detail string) error {
	return &Error{Code: code, Model: model, Capability: capability, Detail: detail}
}

func codedError(code ErrorCode, cause error, detail string) error {
	if cause == nil {
		return newError(code, "", "", detail)
	}
	return fmt.Errorf("%w: %w", newError(code, "", "", detail), cause)
}
