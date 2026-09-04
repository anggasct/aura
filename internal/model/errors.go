package model

import (
	"context"
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
	ErrorCodeFallbackExhausted     ErrorCode = "model_fallback_exhausted"
	ErrorCodeFallbackBoundary      ErrorCode = "model_fallback_boundary"
)

type ErrorClass string

const (
	ErrorClassTransient      ErrorClass = "transient"
	ErrorClassRateLimited    ErrorClass = "rate_limited"
	ErrorClassOverloaded     ErrorClass = "overloaded"
	ErrorClassDeadline       ErrorClass = "deadline"
	ErrorClassAuth           ErrorClass = "auth"
	ErrorClassInvalidRequest ErrorClass = "invalid_request"
	ErrorClassPolicyRejected ErrorClass = "policy_rejected"
	ErrorClassUnsupported    ErrorClass = "unsupported"
	ErrorClassProtocol       ErrorClass = "protocol"
)

func (c ErrorClass) FallbackEligible() bool {
	return c == ErrorClassTransient || c == ErrorClassRateLimited || c == ErrorClassOverloaded
}

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

func ClassifyError(err error) ErrorClass {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return ErrorClassDeadline
	}
	if isTransientError(err) {
		return ErrorClassTransient
	}

	code, hasCode := CodeOf(err)
	if hasCode {
		switch code {
		case ErrorCodeRateLimited:
			return ErrorClassRateLimited
		case ErrorCodeOverloaded:
			return ErrorClassOverloaded
		case ErrorCodeDeadlineExceeded:
			return ErrorClassDeadline
		case ErrorCodeSecretInvalid, ErrorCodeAuthFailed:
			return ErrorClassAuth
		case ErrorCodeContentFiltered:
			return ErrorClassPolicyRejected
		case ErrorCodeCapabilityUnsupported:
			return ErrorClassUnsupported
		case ErrorCodeContextTooLong, ErrorCodeRouteInvalid:
			return ErrorClassInvalidRequest
		case ErrorCodeProtocolInvalid, ErrorCodeFallbackBoundary:
			return ErrorClassProtocol
		case ErrorCodeConnectionFailed, ErrorCodeStreamInvalid:
			return ErrorClassTransient
		case ErrorCodeNotFound:
			return ErrorClassInvalidRequest
		case ErrorCodeBudgetExceeded:
			return ErrorClassDeadline
		case ErrorCodeFallbackExhausted:
			return ErrorClassOverloaded
		}
	}

	if errors.Is(err, ErrRateLimited) {
		return ErrorClassRateLimited
	}
	if errors.Is(err, ErrOverloaded) {
		return ErrorClassOverloaded
	}
	if errors.Is(err, ErrAuthFailed) {
		return ErrorClassAuth
	}
	if errors.Is(err, ErrContentFiltered) {
		return ErrorClassPolicyRejected
	}
	if errors.Is(err, ErrContextTooLong) || errors.Is(err, ErrModelNotFound) || errors.Is(err, ErrInvalidToolCall) {
		return ErrorClassInvalidRequest
	}
	if errors.Is(err, ErrConnectionFailed) || errors.Is(err, ErrStreamIdle) || errors.Is(err, ErrStreamTruncated) {
		return ErrorClassTransient
	}

	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "rate limit") || strings.Contains(msg, "429"):
		return ErrorClassRateLimited
	case strings.Contains(msg, "overloaded") || strings.Contains(msg, "503") || strings.Contains(msg, "502") || strings.Contains(msg, "504") || strings.Contains(msg, "service unavailable"):
		return ErrorClassOverloaded
	case strings.Contains(msg, "unauthorized") || strings.Contains(msg, "forbidden") || strings.Contains(msg, "auth") || strings.Contains(msg, "401") || strings.Contains(msg, "403"):
		return ErrorClassAuth
	case strings.Contains(msg, "deadline") || strings.Contains(msg, "timeout") || strings.Contains(msg, "canceled") || strings.Contains(msg, "cancelled"):
		return ErrorClassDeadline
	case strings.Contains(msg, "policy") || strings.Contains(msg, "filter") || strings.Contains(msg, "safety"):
		return ErrorClassPolicyRejected
	case strings.Contains(msg, "unsupported") || strings.Contains(msg, "capability"):
		return ErrorClassUnsupported
	case strings.Contains(msg, "protocol") || strings.Contains(msg, "invalid json"):
		return ErrorClassProtocol
	case strings.Contains(msg, "connection") || strings.Contains(msg, "reset") || strings.Contains(msg, "broken pipe"):
		return ErrorClassTransient
	default:
		return ErrorClassInvalidRequest
	}
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
