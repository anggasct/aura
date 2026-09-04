package mcp

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrors(t *testing.T) {
	t.Run("nil error CodeOf", func(t *testing.T) {
		code, ok := CodeOf(nil)
		if ok || code != "" {
			t.Fatalf("expected empty, got ok=%v, code=%v", ok, code)
		}
	})

	t.Run("nil Error struct", func(t *testing.T) {
		var e *Error
		if e.Error() != "<nil>" {
			t.Fatalf("expected <nil>, got %s", e.Error())
		}
		if e.Unwrap() != nil {
			t.Fatal("expected nil unwrap")
		}
	})

	t.Run("unrelated error CodeOf", func(t *testing.T) {
		code, ok := CodeOf(errors.New("standard error"))
		if ok || code != "" {
			t.Fatalf("expected false, got ok=%v, code=%v", ok, code)
		}
	})

	t.Run("Errorf and CodeOf", func(t *testing.T) {
		err := Errorf(ErrConfigInvalid, "bad config: %s", "missing name")
		code, ok := CodeOf(err)
		if !ok || code != ErrConfigInvalid {
			t.Fatalf("expected %s, got ok=%v, code=%v", ErrConfigInvalid, ok, code)
		}
		if err.Error() != "mcp_config_invalid: bad config: missing name" {
			t.Fatalf("unexpected message: %s", err.Error())
		}
	})

	t.Run("Wrap and Unwrap", func(t *testing.T) {
		cause := errors.New("underlying network down")
		err := Wrap(ErrServerUnavailable, cause, "connection refused")
		code, ok := CodeOf(err)
		if !ok || code != ErrServerUnavailable {
			t.Fatalf("expected %s, got %s", ErrServerUnavailable, code)
		}
		if !errors.Is(err, cause) {
			t.Fatal("expected errors.Is(err, cause) to be true")
		}
		expectedMsg := fmt.Sprintf("%s: connection refused: %v", ErrServerUnavailable, cause)
		if err.Error() != expectedMsg {
			t.Fatalf("got %q, want %q", err.Error(), expectedMsg)
		}
	})

	t.Run("wrapped in standard fmt.Errorf", func(t *testing.T) {
		orig := Errorf(ErrTrustRequired, "trust required")
		wrapped := fmt.Errorf("context: %w", orig)
		code, ok := CodeOf(wrapped)
		if !ok || code != ErrTrustRequired {
			t.Fatalf("expected %s, got ok=%v, code=%s", ErrTrustRequired, ok, code)
		}
	})
}
