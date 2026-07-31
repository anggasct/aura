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
