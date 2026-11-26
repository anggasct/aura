package skills

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/fault"
)

func runModuleSkillsCmd(t *testing.T, open Opener, args ...string) (string, error) {
	t.Helper()
	cmds := Commands(open)
	if len(cmds) != 1 {
		t.Fatalf("Commands returned %d roots, want 1", len(cmds))
	}
	cmd := cmds[0]
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func isCLIUsageError(err error) bool {
	var ue interface{ UsageError() bool }
	return errors.As(err, &ue) && ue.UsageError()
}

func TestSkillsCommandsRefuseNilOpener(t *testing.T) {
	for _, args := range [][]string{
		{"list"},
		{"show", "local/a@0123456789ab"},
		{"validate", "/tmp"},
		{"create", "--name", "a", "--description", "desc"},
		{"review", "local/a@0123456789ab"},
		{"accept", "local/a@0123456789ab", "--digest", "abc"},
		{"reject", "local/a@0123456789ab"},
		{"disable", "local/a@0123456789ab"},
		{"doctor"},
	} {
		t.Run(args[0], func(t *testing.T) {
			_, err := runModuleSkillsCmd(t, nil, args...)
			if !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("args %v: err = %v, want %s", args, err, ErrorCodeInvalidArgument)
			}
		})
	}
}

func TestSkillsCommandsUsageErrors(t *testing.T) {
	open := func(_ *cobra.Command, _ func(context.Context, *Engine, string) error) error {
		t.Fatal("opener must not be called on usage error")
		return nil
	}
	cases := map[string][]string{
		"bad state":           {"list", "--state", "flying"},
		"create missing desc": {"create", "--name", "a"},
		"create missing name": {"create", "--description", "d"},
		"accept missing dig":  {"accept", "local/a@0123456789ab"},
		"show missing arg":    {"show"},
		"show blank arg":      {"show", "   "},
		"validate extra arg":  {"validate", "a", "b"},
		"review missing arg":  {"review"},
		"reject missing arg":  {"reject"},
		"disable missing arg": {"disable"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := runModuleSkillsCmd(t, open, args...)
			if err == nil || !isCLIUsageError(err) {
				t.Fatalf("args %v: err = %v, want UsageError", args, err)
			}
			var flagged interface{ IsUsageError() bool }
			if !errors.As(err, &flagged) || !flagged.IsUsageError() {
				t.Fatalf("args %v: err = %T does not satisfy IsUsageError()", args, err)
			}
		})
	}
}

func TestSkillsCommandsSubcommandLifecycle(t *testing.T) {
	root := t.TempDir()
	registry := &fakeRegistry{}
	engine := newTestEngine(t, registry, root)
	open := func(cmd *cobra.Command, fn func(context.Context, *Engine, string) error) error {
		return fn(cmd.Context(), engine, root)
	}

	out, err := runModuleSkillsCmd(t, open, "list")
	if err != nil || strings.TrimSpace(out) != "no skills" {
		t.Fatalf("list empty = %q, %v", out, err)
	}

	out, err = runModuleSkillsCmd(t, open, "create", "--name", "draft", "--description", "Draft skill.")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var id, digest string
	for line := range strings.Lines(out) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "id: "); ok {
			id = v
		}
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "digest: "); ok {
			digest = v
		}
	}
	if id == "" || digest == "" {
		t.Fatalf("create out = %q", out)
	}

	out, err = runModuleSkillsCmd(t, open, "accept", id, "--digest", digest)
	if err != nil || strings.TrimSpace(out) != "accepted: "+id {
		t.Fatalf("accept = %q, %v", out, err)
	}

	out, err = runModuleSkillsCmd(t, open, "disable", id)
	if err != nil || strings.TrimSpace(out) != "disabled: "+id {
		t.Fatalf("disable = %q, %v", out, err)
	}

	validDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(validDir, "SKILL.md"), []byte("---\nname: ok\ndescription: Fine.\n---\nBody.\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err = runModuleSkillsCmd(t, open, "validate", validDir)
	if err != nil || !strings.HasPrefix(strings.TrimSpace(out), "valid: ") {
		t.Fatalf("validate = %q, %v", out, err)
	}

	out, err = runModuleSkillsCmd(t, open, "doctor")
	if err != nil || !strings.Contains(out, id) {
		t.Fatalf("doctor = %q, %v", out, err)
	}
}
