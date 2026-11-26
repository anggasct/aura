package shellexec

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/command"
)

type testUsageError struct {
	err error
}

func (e *testUsageError) Error() string { return e.err.Error() }

func (e *testUsageError) Unwrap() error { return e.err }

func TestCommandsExecStubFailsLoudly(t *testing.T) {
	root := &cobra.Command{Use: "aura", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(Commands(nil)...)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"exec", "--", "ls", "-la"})

	err := root.Execute()
	if err == nil {
		t.Fatal("exec succeeded, want not-yet-implemented error")
	}
	if command.IsUsageError(err) {
		t.Fatalf("exec RunE error %v should not be a usage error", err)
	}
	if err.Error() != "aura exec: sandboxed execution not yet implemented" {
		t.Fatalf("exec error = %q, want loud unimplemented message", err.Error())
	}
}

func TestCommandsExecRequiresArgument(t *testing.T) {
	t.Run("default usage wrapper", func(t *testing.T) {
		root := &cobra.Command{Use: "aura", SilenceUsage: true, SilenceErrors: true}
		root.AddCommand(Commands(nil)...)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs([]string{"exec"})

		err := root.Execute()
		if err == nil {
			t.Fatal("exec without arguments succeeded, want usage error")
		}
		if !command.IsUsageError(err) {
			t.Fatalf("exec without arguments err = %T (%v), want command.UsageError", err, err)
		}
		if !strings.Contains(err.Error(), "requires at least one argument") {
			t.Fatalf("exec usage error = %q, want missing argument message", err.Error())
		}
	})

	t.Run("custom usage wrapper", func(t *testing.T) {
		wrapped := false
		root := &cobra.Command{Use: "aura", SilenceUsage: true, SilenceErrors: true}
		root.AddCommand(Commands(func(err error) error {
			wrapped = true
			return &testUsageError{err: err}
		})...)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs([]string{"exec"})

		err := root.Execute()
		if err == nil {
			t.Fatal("exec without arguments succeeded, want usage error")
		}
		if !wrapped {
			t.Fatal("custom wrapUsage was not called")
		}
		var target *testUsageError
		if !errors.As(err, &target) {
			t.Fatalf("err = %T (%v), want *testUsageError", err, err)
		}
	})
}
