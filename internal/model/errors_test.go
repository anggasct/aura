package model

import (
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
