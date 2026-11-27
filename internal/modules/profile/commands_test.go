package profile

import (
	"bytes"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
)

func TestProfileCommandsRefuseNilOpener(t *testing.T) {
	cases := [][]string{
		{"list"},
		{"show", "pf-1"},
		{"review"},
		{"accept", "pf-1"},
		{"reject", "pf-1"},
		{"set", "--category", "tool", "--key", "editor", "--value", "neovim"},
		{"delete", "pf-1", "--force"},
	}
	for _, args := range cases {
		t.Run(args[0], func(t *testing.T) {
			cmds := Commands(nil)
			if len(cmds) != 1 {
				t.Fatalf("Commands returned %d root commands, want 1", len(cmds))
			}
			cmd := cmds[0]
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(args)
			err := cmd.ExecuteContext(t.Context())
			if !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("args %v err = %v, want %q", args, err, ErrorCodeInvalidArgument)
			}
		})
	}
}
