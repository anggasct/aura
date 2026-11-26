package model

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/fault"
)

func TestCommandsNilOpenerReturnsInvalidArgument(t *testing.T) {
	cmds := Commands(nil)
	if len(cmds) != 1 || cmds[0].Name() != "models" {
		t.Fatalf("Commands(nil) = %v, want single models command", cmds)
	}
	cases := [][]string{
		{"routes"},
		{"circuits"},
		{"circuit-reset", "primary"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			root := Commands(nil)[0]
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(args)
			err := root.ExecuteContext(t.Context())
			if !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("Execute(%v) = %v, want %s", args, err, ErrorCodeInvalidArgument)
			}
		})
	}
}

func TestModelsRoutesPrintsConfiguredRoutes(t *testing.T) {
	open := func(_ *cobra.Command, needDB bool, fn func(context.Context, *CommandEnv) error) error {
		if needDB {
			t.Fatal("routes should not request a database")
		}
		return fn(t.Context(), &CommandEnv{
			Routes: map[string]ModelRoute{
				"fast": {Candidates: []string{"primary", "backup"}},
			},
		})
	}
	cmd := Commands(open)[0]
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"routes"})
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("models routes: %v", err)
	}
	if !strings.Contains(out.String(), "fast") || !strings.Contains(out.String(), "primary, backup") {
		t.Fatalf("output = %q", out.String())
	}
}
