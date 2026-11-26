package agent

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/clikit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
)

func TestCommandsNilOpenerReturnsInvalidArgument(t *testing.T) {
	cases := []struct {
		top  string
		args []string
	}{
		{"agents", []string{"list"}},
		{"agents", []string{"show", "main"}},
		{"persona", []string{"list"}},
		{"bindings", []string{"explain", "--account", "main"}},
	}
	for _, tc := range cases {
		t.Run(tc.top+"_"+strings.Join(tc.args, "_"), func(t *testing.T) {
			root := &cobra.Command{Use: "aura", SilenceUsage: true, SilenceErrors: true}
			root.AddCommand(Commands(nil)...)
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(append([]string{tc.top}, tc.args...))
			err := root.ExecuteContext(t.Context())
			if !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("Execute(%s %v) = %v, want %s", tc.top, tc.args, err, ErrorCodeInvalidArgument)
			}
		})
	}
}

func TestBindingsExplainExitsThreeWhenUnmatched(t *testing.T) {
	registry, err := Build(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	open := func(_ *cobra.Command, fn func(context.Context, *CommandEnv) error) error {
		return fn(t.Context(), &CommandEnv{
			Registry: registry,
			Accounts: []ChannelAccount{{Channel: BindingChannelDiscord, Name: "office"}},
		})
	}
	root := &cobra.Command{Use: "aura", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(Commands(open)...)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"bindings", "explain", "--account", "office"})
	err = root.ExecuteContext(t.Context())
	code, ok := clikit.ExitCodeOf(err)
	if !ok || code != ExitNoPersona {
		t.Fatalf("bindings explain err = %v (code=%d ok=%v), want exit %d", err, code, ok, ExitNoPersona)
	}
	if strings.TrimSpace(out.String()) != "no persona" {
		t.Fatalf("stdout = %q, want no persona", out.String())
	}
}

func TestBindingsExplainUsageError(t *testing.T) {
	root := &cobra.Command{Use: "aura", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(Commands(nil)...)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"bindings", "explain"})
	err := root.ExecuteContext(t.Context())
	if !clikit.IsUsageError(err) {
		t.Fatalf("bindings explain without --account = %v, want clikit.UsageError", err)
	}
}

func TestPolicyAndPersonaNilGuards(t *testing.T) {
	var nilPolicy *AccessPolicy
	if ceiling, ok := nilPolicy.ToolCeiling("main", principal.ClassTrusted); ok || ceiling != nil {
		t.Fatalf("nil ToolCeiling = (%v, %v), want (nil, false)", ceiling, ok)
	}
	if _, err := nilPolicy.StandingGrant(t.Context(), "discord:1", "main", "read_file", ""); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("nil StandingGrant = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	emptyPolicy := &AccessPolicy{}
	if _, err := emptyPolicy.StandingGrant(t.Context(), "discord:1", "main", "read_file", ""); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("empty StandingGrant = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	var emptyPersonas RegistryPersonas
	if _, ok := emptyPersonas.Fingerprint("main"); ok {
		t.Fatal("empty RegistryPersonas.Fingerprint = true, want false")
	}
	if got := emptyPersonas.Knowledge("main", principal.ClassOwner); got != nil {
		t.Fatalf("empty RegistryPersonas.Knowledge = %v, want nil", got)
	}
	LogPersonas(t.Context(), nil, nil)
	if _, err := ResolveChatPersona(t.Context(), nil, nil, "", ""); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("ResolveChatPersona(nil) = %v, want %s", err, ErrorCodeInvalidArgument)
	}
}
