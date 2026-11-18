package command

import (
	"errors"
	"fmt"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
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
			got, ok := fault.CodeOf[ErrorCode](err)
			if !ok || got != code {
				t.Fatalf("CodeOf = %q, %v, want %q, true", got, ok, code)
			}
			wrapped := fmt.Errorf("switch failed: %w", err)
			got, ok = fault.CodeOf[ErrorCode](wrapped)
			if !ok || got != code {
				t.Fatalf("fault.CodeOf[ErrorCode](wrapped) = %q, %v, want %q, true", got, ok, code)
			}
		})
	}
}

func TestCodeOfRejectsForeignAndNilErrors(t *testing.T) {
	if got, ok := fault.CodeOf[ErrorCode](nil); ok || got != "" {
		t.Errorf("fault.CodeOf[ErrorCode](nil) = %q, %v, want \"\", false", got, ok)
	}
	foreign := fmt.Errorf("wrapped foreign: %w", errors.New("foreign"))
	if got, ok := fault.CodeOf[ErrorCode](foreign); ok || got != "" {
		t.Errorf("fault.CodeOf[ErrorCode](foreign) = %q, %v, want \"\", false", got, ok)
	}
}
