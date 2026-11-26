package effect

import (
	"bytes"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/clikit"
	"github.com/anggasct/aura/internal/platform/fault"
)

func TestCommandsNilOpenerReturnsInvalidArgument(t *testing.T) {
	cases := [][]string{
		{"approve", "eff-1", "--action", "retry", "--reason", "test"},
		{"list"},
		{"reconcile", "eff-1"},
		{"mark", "eff-1", "--succeeded", "--reason", "ok", "--approval-token", "tok"},
		{"retry", "eff-1", "--approval-token", "tok"},
		{"prune"},
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

func TestEffectCommandsUsageErrors(t *testing.T) {
	cases := [][]string{
		{"approve", "eff-1", "--action", "bogus", "--reason", "ok"},
		{"list", "--limit", "-1"},
		{"mark", "eff-1", "--reason", "ok", "--approval-token", "tok"},
		{"retry", "eff-1"},
		{"prune", "--before", "not-a-time"},
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
