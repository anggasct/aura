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

func previewFirstRig(t *testing.T) *operationtest.Rig {
	t.Helper()
	catalog := operationtest.Catalog()
	descriptorNamed(&catalog, operationtest.Grant).PreviewFirst = true
	return operationtest.NewRig(t, &operationtest.Options{Catalogs: []operation.Catalog{catalog}})
}

func TestAPreviewFirstOperationShowsItsPreviewOnTheOwnerSurfacesUntilApplied(t *testing.T) {
	t.Run("the CLI", func(t *testing.T) {
		rig := previewFirstRig(t)
		catalog := operationtest.Catalog()
		descriptorNamed(&catalog, operationtest.Grant).PreviewFirst = true
		commands, err := operation.Commands([]operation.Catalog{catalog}, func(_ *cobra.Command, _ *operation.Descriptor, run func(ctx context.Context, invoker operation.Invoker) error) error {
			return run(t.Context(), rig.Registry)
		})
		if err != nil {
			t.Fatalf("Commands: %v", err)
		}
		root := &cobra.Command{Use: "aura", SilenceUsage: true, SilenceErrors: true}
		root.AddCommand(commands...)
		cli := &cliRig{rig: rig, root: root}

		out, err := cli.run(t, "stub", "grant", "alpha")

		if err != nil || !strings.Contains(out, "subject: alpha") {
			t.Fatalf("without apply = %q, %v; want the preview", out, err)
		}
		if rig.Stubs.Handled(operationtest.Grant) != 0 {
			t.Fatalf("the preview ran the operation %d times", rig.Stubs.Handled(operationtest.Grant))
		}
		if out, err = cli.run(t, "stub", "grant", "alpha", "--apply"); err != nil || !strings.Contains(out, "done: true") {
			t.Fatalf("with apply = %q, %v; want the operation to run", out, err)
		}
		if rig.Stubs.Handled(operationtest.Grant) != 1 {
			t.Fatalf("handled = %d, want 1", rig.Stubs.Handled(operationtest.Grant))
		}
	})
	t.Run("chat", func(t *testing.T) {
		rig := previewFirstRig(t)
		chat, err := operation.NewChat(&operation.ChatDeps{Registry: rig.Registry, Capabilities: capabilitytest.Host(t)})
		if err != nil {
			t.Fatalf("NewChat: %v", err)
		}

		preview := runChat(t, chat, typed{"grant set1", "telegram:1"})

		if preview.Code != command.ErrorCodeOK || !slices.Equal(preview.Lines, []string{"subject: set1", "cannot be undone"}) || rig.Stubs.Handled(operationtest.Grant) != 0 {
			t.Fatalf("preview = %+v handled %d, want the preview lines and no run", preview, rig.Stubs.Handled(operationtest.Grant))
		}
		applied := runChat(t, chat, typed{"grant set1 --apply", "telegram:2"})
		if applied.Code != command.ErrorCodeOK || rig.Stubs.Handled(operationtest.Grant) != 1 {
			t.Fatalf("applied = %+v handled %d, want one run", applied, rig.Stubs.Handled(operationtest.Grant))
		}
	})
	t.Run("the agent keeps its card", func(t *testing.T) {
		rig := previewFirstRig(t)

		outcome := rig.Execute(t.Context(), operationtest.Request(t, operationtest.ToolOf(operationtest.Grant), "alpha"))

		if outcome.Class != toolbroker.ResultOK || len(rig.Prompts()) != 1 || rig.Stubs.Handled(operationtest.Grant) != 1 {
			t.Fatalf("outcome = %+v prompts %d handled %d, want one card and one run", outcome, len(rig.Prompts()), rig.Stubs.Handled(operationtest.Grant))
		}
	})
}
