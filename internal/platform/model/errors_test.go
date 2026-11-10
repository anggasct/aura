package model

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestErrorSentinels_DetectableWhenWrapped(t *testing.T) {
	sentinels := []error{
		ErrRateLimited, ErrAuthFailed, ErrModelNotFound, ErrOverloaded,
		ErrContentFiltered, ErrContextTooLong, ErrInvalidToolCall,
		ErrConnectionFailed, ErrStreamIdle,
	}
	for _, sentinel := range sentinels {
		wrapped := fmt.Errorf("provider anthropic: %w", sentinel)
		if !errors.Is(wrapped, sentinel) {
			t.Errorf("errors.Is failed to detect wrapped %v", sentinel)
		}
	}
}

func TestStructuredErrorsExposeStableCodes(t *testing.T) {
	err := newError(ErrorCodeCapabilityUnsupported, "primary", "vision", "capability is unavailable")
	if code, ok := CodeOf(fmt.Errorf("validate model: %w", err)); !ok || code != ErrorCodeCapabilityUnsupported {
		t.Fatalf("CodeOf(%v) = %q, %v", err, code, ok)
	}
}

func TestClassifyError_IgnoresMessageText(t *testing.T) {
	untyped := []error{
		errors.New("request rejected by the provider's usage policy"),
		errors.New("429 too many requests"),
		errors.New("user is unauthorized for this resource"),
		errors.New("upstream request timed out"),
		errors.New("connection reset by peer"),
		errors.New("the model does not support this capability"),
	}
	for _, err := range untyped {
		class := ClassifyError(err)
		if class.FallbackEligible() {
			t.Errorf("ClassifyError(%q) = %q, which is fallback-eligible; untyped errors must fail closed", err, class)
		}
		if class != ErrorClassInvalidRequest {
			t.Errorf("ClassifyError(%q) = %q, want invalid_request (text-based classification)", err, class)
		}
	}

	if got := ClassifyError(nil); got != ErrorClassInvalidRequest {
		t.Errorf("ClassifyError(nil) = %q, want invalid_request", got)
	}

	typed := []struct {
		err  error
		want ErrorClass
	}{
		{newError(ErrorCodeContentFiltered, "", "", "blocked"), ErrorClassPolicyRejected},
		{fmt.Errorf("wrapped: %w", newError(ErrorCodeRateLimited, "", "", "slow down")), ErrorClassRateLimited},
		{fmt.Errorf("wrapped: %w", ErrAuthFailed), ErrorClassAuth},
		{fmt.Errorf("wrapped: %w", context.DeadlineExceeded), ErrorClassDeadline},
	}
	for _, tc := range typed {
		if got := ClassifyError(tc.err); got != tc.want {
			t.Errorf("ClassifyError(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
