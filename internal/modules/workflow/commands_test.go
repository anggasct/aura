package workflow

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/fault"
)

func TestCommandsRejectNilOpener(t *testing.T) {
	cmd := Commands(nil)[0]
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"runs"})
	err := cmd.ExecuteContext(t.Context())
	if !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("Execute(nil opener) = %v, want %s", err, ErrorCodeInvalidArgument)
	}
}

func TestWorkflowProposalsInvalidStatusIsUsageError(t *testing.T) {
	called := false
	cmd := Commands(func(_ *cobra.Command, _ CommandMode, _ func(context.Context, *CommandDeps) error) error {
		called = true
		return nil
	})[0]
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"proposals", "--status", "invalid"})
	err := cmd.ExecuteContext(t.Context())
	if err == nil {
		t.Fatal("proposals --status invalid succeeded")
	}
	if called {
		t.Fatal("opener was called despite invalid --status")
	}
	if !strings.Contains(err.Error(), "--status must be proposed, approved, rejected or expired") {
		t.Fatalf("err = %q, want status usage message", err.Error())
	}
	var flagged interface{ IsUsageError() bool }
	if !errors.As(err, &flagged) || !flagged.IsUsageError() {
		t.Fatalf("err %T does not implement IsUsageError() bool", err)
	}
}
