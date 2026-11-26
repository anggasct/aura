package clikit

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/spf13/cobra"
)

func TestUsageError(t *testing.T) {
	if err := NewUsageError(nil); err != nil {
		t.Fatalf("NewUsageError(nil) = %v, want nil", err)
	}
	cause := errors.New("missing flag")
	wrapped := fmt.Errorf("outer: %w", NewUsageError(cause))
	if !IsUsageError(wrapped) {
		t.Fatalf("IsUsageError(%v) = false, want true", wrapped)
	}
	if !errors.Is(wrapped, cause) {
		t.Fatalf("errors.Is(%v, cause) = false, want true", wrapped)
	}
	if IsUsageError(cause) {
		t.Fatalf("IsUsageError(cause) = true, want false")
	}
}

func TestExitCodeError(t *testing.T) {
	if err := NewExitCodeError(3, nil); err != nil {
		t.Fatalf("NewExitCodeError(3, nil) = %v, want nil", err)
	}
	cause := errors.New("no persona")
	wrapped := fmt.Errorf("outer: %w", NewExitCodeError(3, cause))
	code, ok := ExitCodeOf(wrapped)
	if !ok || code != 3 {
		t.Fatalf("ExitCodeOf = (%d, %v), want (3, true)", code, ok)
	}
	if !errors.Is(wrapped, cause) {
		t.Fatalf("errors.Is(%v, cause) = false, want true", wrapped)
	}
	if _, ok := ExitCodeOf(cause); ok {
		t.Fatal("ExitCodeOf(cause) = true, want false")
	}
}

func TestTableAndWriteLines(t *testing.T) {
	var buf bytes.Buffer
	tbl := NewTable(&buf)
	tbl.Printf("A\tB\n")
	tbl.Printf("one\t2\n")
	if err := tbl.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("one")) {
		t.Fatalf("table output = %q", buf.String())
	}

	cmd := &cobra.Command{Use: "demo"}
	buf.Reset()
	cmd.SetOut(&buf)
	if err := WriteLines(cmd, []string{"first", "second"}); err != nil {
		t.Fatalf("WriteLines: %v", err)
	}
	if buf.String() != "first\nsecond\n" {
		t.Fatalf("WriteLines output = %q", buf.String())
	}
	if err := NoPositionalArgs(cmd, nil); err != nil {
		t.Fatalf("NoPositionalArgs(nil) = %v", err)
	}
	if err := NoPositionalArgs(cmd, []string{"extra"}); !IsUsageError(err) {
		t.Fatalf("NoPositionalArgs(extra) = %v, want UsageError", err)
	}
}

func TestErrorNilReceiversAndNilInnerDoNotPanic(t *testing.T) {
	var nilUsage *UsageError
	if nilUsage.Error() != "" || nilUsage.Unwrap() != nil {
		t.Fatalf("nil UsageError = (%q, %v)", nilUsage.Error(), nilUsage.Unwrap())
	}
	emptyUsage := &UsageError{}
	if emptyUsage.Error() != "" || emptyUsage.Unwrap() != nil {
		t.Fatalf("empty UsageError = (%q, %v)", emptyUsage.Error(), emptyUsage.Unwrap())
	}
	if IsUsageError(nilUsage) {
		t.Fatal("IsUsageError(typed nil) = true, want false")
	}

	var nilExit *ExitCodeError
	if nilExit.Error() != "" || nilExit.Unwrap() != nil || nilExit.ExitCode() != 0 {
		t.Fatalf("nil ExitCodeError = (%q, %v, %d)", nilExit.Error(), nilExit.Unwrap(), nilExit.ExitCode())
	}
	emptyExit := &ExitCodeError{Code: 3}
	if emptyExit.Error() != "" || emptyExit.Unwrap() != nil || emptyExit.ExitCode() != 3 {
		t.Fatalf("empty ExitCodeError = (%q, %v, %d)", emptyExit.Error(), emptyExit.Unwrap(), emptyExit.ExitCode())
	}
	if _, ok := ExitCodeOf(nilExit); ok {
		t.Fatal("ExitCodeOf(typed nil) = true, want false")
	}
}
