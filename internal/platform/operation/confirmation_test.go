package operation_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

type confirmationCase struct {
	name         string
	id           string
	verb         string
	confirmation operation.OwnerConfirmation
	wantPreview  bool
}

var confirmationCases = []confirmationCase{
	{name: "a trust grant that runs directly", id: operationtest.Grant, verb: "grant", confirmation: operation.ConfirmRunDirectly},
	{name: "a trust grant that previews until apply", id: operationtest.Grant, verb: "grant", confirmation: operation.ConfirmPreviewUntilApply, wantPreview: true},
	{name: "a change that runs directly", id: operationtest.Change, verb: "change", confirmation: operation.ConfirmRunDirectly},
	{name: "a change that previews until apply", id: operationtest.Change, verb: "change", confirmation: operation.ConfirmPreviewUntilApply, wantPreview: true},
	{name: "a destructive operation with no choice", id: operationtest.Delete, verb: "delete", wantPreview: true},
	{name: "a destructive operation that previews explicitly", id: operationtest.Delete, verb: "delete", confirmation: operation.ConfirmPreviewUntilApply, wantPreview: true},
}

func confirmationRig(t *testing.T, tc *confirmationCase) (*operationtest.Rig, []operation.Catalog) {
	t.Helper()
	catalog := operationtest.Catalog()
	descriptor := descriptorNamed(&catalog, tc.id)
	descriptor.Preview, descriptor.Confirmation = true, tc.confirmation
	catalogs := []operation.Catalog{catalog}
	rig := operationtest.NewRig(t, &operationtest.Options{
		Catalogs: catalogs,
		Bindings: func(stubs *operationtest.Stubs) []operation.Binding {
			bindings := stubs.Bindings()
			for i := range bindings {
				if bindings[i].ID == tc.id && bindings[i].Preview == nil {
					bindings[i].Preview = bindings[slices.IndexFunc(bindings, func(b operation.Binding) bool { return b.ID == operationtest.Delete })].Preview
				}
			}
			return bindings
		},
	})
	return rig, catalogs
}

func TestTheOwnerConfirmationDecidesWhetherTheCLIPreviewsUntilApplied(t *testing.T) {
	for _, tc := range confirmationCases {
		t.Run(tc.name, func(t *testing.T) {
			rig, catalogs := confirmationRig(t, &tc)
			commands, err := operation.Commands(catalogs, func(_ *cobra.Command, _ *operation.Descriptor, run func(ctx context.Context, invoker operation.Invoker) error) error {
				return run(t.Context(), rig.Registry)
			})
			if err != nil {
				t.Fatalf("Commands: %v", err)
			}
			root := &cobra.Command{Use: "aura", SilenceUsage: true, SilenceErrors: true}
			root.AddCommand(commands...)
			cli := &cliRig{rig: rig, root: root}

			out, err := cli.run(t, "stub", tc.verb, "alpha")

			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := rig.Stubs.Handled(tc.id); (got == 0) != tc.wantPreview {
				t.Fatalf("handled = %d after one message, want a preview only = %v (output %q)", got, tc.wantPreview, out)
			}
			if tc.wantPreview {
				if !strings.Contains(out, "subject: alpha") {
					t.Errorf("output %q is not the preview", out)
				}
				if out, err = cli.run(t, "stub", tc.verb, "alpha", "--apply"); err != nil || !strings.Contains(out, "done: true") || rig.Stubs.Handled(tc.id) != 1 {
					t.Fatalf("with apply = %q, %v, handled %d; want one run", out, err, rig.Stubs.Handled(tc.id))
				}
			}
		})
	}
}

func TestTheOwnerConfirmationDecidesWhetherChatPreviewsUntilApplied(t *testing.T) {
	for _, tc := range confirmationCases {
		t.Run(tc.name, func(t *testing.T) {
			rig, _ := confirmationRig(t, &tc)
			chat, err := operation.NewChat(&operation.ChatDeps{Registry: rig.Registry, Capabilities: capabilitytest.Host(t)})
			if err != nil {
				t.Fatalf("NewChat: %v", err)
			}

			first := runChat(t, chat, typed{tc.verb + " set1", "telegram:1"})

			if first.Code != command.ErrorCodeOK {
				t.Fatalf("reply = %+v", first)
			}
			if got := rig.Stubs.Handled(tc.id); (got == 0) != tc.wantPreview {
				t.Fatalf("handled = %d after one message, want a preview only = %v", got, tc.wantPreview)
			}
			if tc.wantPreview {
				if !slices.Equal(first.Lines, []string{"subject: set1", "cannot be undone"}) {
					t.Errorf("preview = %q", first.Lines)
				}
				if applied := runChat(t, chat, typed{tc.verb + " set1 --apply", "telegram:2"}); applied.Code != command.ErrorCodeOK || rig.Stubs.Handled(tc.id) != 1 {
					t.Fatalf("applied = %+v handled %d, want one run", applied, rig.Stubs.Handled(tc.id))
				}
			}
		})
	}
}

func TestTheAgentCardIsUnaffectedByTheOwnerConfirmation(t *testing.T) {
	for _, confirmation := range []operation.OwnerConfirmation{operation.ConfirmRunDirectly, operation.ConfirmPreviewUntilApply} {
		t.Run(string(confirmation), func(t *testing.T) {
			rig, _ := confirmationRig(t, &confirmationCase{id: operationtest.Grant, confirmation: confirmation})

			outcome := rig.Execute(t.Context(), operationtest.Request(t, operationtest.ToolOf(operationtest.Grant), "alpha"))

			if outcome.Class != toolbroker.ResultOK || len(rig.Prompts()) != 1 || rig.Stubs.Handled(operationtest.Grant) != 1 {
				t.Fatalf("outcome = %+v prompts %d handled %d, want one card and one run", outcome, len(rig.Prompts()), rig.Stubs.Handled(operationtest.Grant))
			}
		})
	}
}
