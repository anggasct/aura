package usage

import (
	"bytes"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/clikit"
	"github.com/anggasct/aura/internal/platform/fault"
)

func TestCommandsNilOpenerReturnsInvalidArgument(t *testing.T) {
	cases := [][]string{
		{"status"},
		{"entries"},
		{"prices"},
		{"reconcile"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
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

func TestUsageCommandsUsageErrors(t *testing.T) {
	cases := [][]string{
		{"status", "--by", "bogus"},
		{"entries", "--limit", "-1"},
		{"prices", "extra"},
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
