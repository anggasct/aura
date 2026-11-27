package storage

import (
	"bytes"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/clikit"
	"github.com/anggasct/aura/internal/platform/fault"
)

func TestCommandsNilOpenerReturnsInvalidArgument(t *testing.T) {
	cases := [][]string{
		{"backup"},
		{"upgrade"},
		{"verify", "--input", "/tmp/b"},
		{"restore", "--input", "/tmp/b"},
		{"reconcile"},
		{"gc"},
	}
	for _, args := range cases {
		t.Run(args[0], func(t *testing.T) {
			cmd := Commands(nil)[0]
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(args)
			err := cmd.ExecuteContext(t.Context())
			if !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("Execute(%v) = %v, want %s", args, err, ErrorCodeInvalidArgument)
			}
		})
	}
}

func TestStorageCommandsUsageErrors(t *testing.T) {
	cases := [][]string{
		{"verify"},
		{"restore"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			cmd := Commands(nil)[0]
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(args)
			err := cmd.ExecuteContext(t.Context())
			if !clikit.IsUsageError(err) {
				t.Fatalf("Execute(%v) = %v, want clikit.UsageError", args, err)
			}
		})
	}
}
