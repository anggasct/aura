package command

import (
	"errors"
	"fmt"
	"testing"
)

func TestCodeOfRoundTripsEveryErrorfCode(t *testing.T) {
	codes := []ErrorCode{
		ErrorCodeUnknown,
		ErrorCodeUsage,
		ErrorCodeDenied,
		ErrorCodeUnavailable,
		ErrorCodeScope,
		ErrorCodeNotSwitchable,
		ErrorCodeSkillUnavailable,
		ErrorCodeFailed,
		ErrorCodeInvalidArgument,
	}
	for _, code := range codes {
		t.Run(string(code), func(t *testing.T) {
			err := Errorf(code, "detail %d", 7)
			got, ok := CodeOf(err)
			if !ok || got != code {
				t.Fatalf("CodeOf = %q, %v, want %q, true", got, ok, code)
			}
			wrapped := fmt.Errorf("switch failed: %w", err)
			got, ok = CodeOf(wrapped)
			if !ok || got != code {
				t.Fatalf("CodeOf(wrapped) = %q, %v, want %q, true", got, ok, code)
			}
		})
	}
}

func TestCodeOfRejectsForeignAndNilErrors(t *testing.T) {
	if got, ok := CodeOf(nil); ok || got != "" {
		t.Errorf("CodeOf(nil) = %q, %v, want \"\", false", got, ok)
	}
	foreign := fmt.Errorf("wrapped foreign: %w", errors.New("foreign"))
	if got, ok := CodeOf(foreign); ok || got != "" {
		t.Errorf("CodeOf(foreign) = %q, %v, want \"\", false", got, ok)
	}
}
