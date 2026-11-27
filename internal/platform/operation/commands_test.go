package operation_test

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/clikit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
)

type cliRig struct {
	rig  *operationtest.Rig
	root *cobra.Command
}

func newCLIRig(t *testing.T) *cliRig {
	t.Helper()
	rig := operationtest.NewRig(t, nil)
	open := func(_ *cobra.Command, _ *operation.Descriptor, run func(ctx context.Context, invoker operation.Invoker) error) error {
		return run(t.Context(), rig.Registry)
	}
	commands, err := operation.Commands([]operation.Catalog{operationtest.Catalog()}, open)
	if err != nil {
		t.Fatalf("Commands: %v", err)
	}
	root := &cobra.Command{Use: "aura", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(commands...)
	return &cliRig{rig: rig, root: root}
}

func (c *cliRig) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	c.root.SetOut(&out)
	c.root.SetErr(&out)
	c.root.SetArgs(args)
	err := c.root.ExecuteContext(t.Context())
	return out.String(), err
}

func TestCommandsGenerateTheTreeFromTheDescriptors(t *testing.T) {
	cli := newCLIRig(t)
	group, _, err := cli.root.Find([]string{"stub"})
	if err != nil || group.Name() != "stub" {
		t.Fatalf("Find(stub) = %v, %v", group, err)
	}
	var verbs []string
	for _, command := range group.Commands() {
		verbs = append(verbs, command.Name())
		if command.Annotations[operation.AnnotationOperation] == "" {
			t.Errorf("command %s has no operation annotation", command.Name())
		}
	}
	slices.Sort(verbs)
	want := []string{"change", "delete", "external", "fresh", "grant", "host", "idempotent", "notice", "owner-only", "read"}
	if !slices.Equal(verbs, want) {
		t.Fatalf("stub verbs = %v, want %v", verbs, want)
	}
}

func TestCommandsRejectAnIdThatIsBothACommandAndAGroup(t *testing.T) {
	catalog := operationtest.Catalog()
	nested := catalog.Descriptors[0]
	nested.ID = operationtest.Read + ".more"
	catalog.Descriptors = append(catalog.Descriptors, nested)
	open := func(*cobra.Command, *operation.Descriptor, func(context.Context, operation.Invoker) error) error {
		return nil
	}
	_, err := operation.Commands([]operation.Catalog{catalog}, open)
	if !fault.HasCode(err, operation.ErrorCodeInvalid) {
		t.Fatalf("Commands = %v, want operation_invalid", err)
	}
}

func TestCommandsBuildFromCopiedDescriptors(t *testing.T) {
	catalog := operationtest.Catalog()
	opened := ""
	open := func(_ *cobra.Command, descriptor *operation.Descriptor, run func(context.Context, operation.Invoker) error) error {
		opened = descriptor.Summary
		return nil
	}
	commands, err := operation.Commands([]operation.Catalog{catalog}, open)
	if err != nil {
		t.Fatalf("Commands: %v", err)
	}
	catalog.Descriptors[0].Summary = "changed after the tree was built"
	root := &cobra.Command{Use: "aura", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(commands...)
	root.SetArgs([]string{"stub", "read", "x"})
	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if opened != operationtest.DefaultNote {
		t.Fatalf("the command saw the summary %q, want the copy taken at build time", opened)
	}
}

func TestOperationCommandOutput(t *testing.T) {
	t.Run("a record prints key value lines", func(t *testing.T) {
		out, err := newCLIRig(t).run(t, "stub", "read", "alpha")
		if err != nil || out != "done: true\noperation: stub.read\n" {
			t.Fatalf("output = %q, err = %v", out, err)
		}
	})
	t.Run("json prints the data object", func(t *testing.T) {
		out, err := newCLIRig(t).run(t, "stub", "read", "alpha", "--json")
		if err != nil || out != "{\n  \"done\": true,\n  \"operation\": \"stub.read\"\n}\n" {
			t.Fatalf("output = %q, err = %v", out, err)
		}
	})
	t.Run("a missing required parameter is a usage error", func(t *testing.T) {
		_, err := newCLIRig(t).run(t, "stub", "read")
		if !clikit.IsUsageError(err) {
			t.Fatalf("err = %v, want a usage error", err)
		}
	})
	t.Run("an unknown flag is a usage error", func(t *testing.T) {
		_, err := newCLIRig(t).run(t, "stub", "read", "alpha", "--nope")
		if err == nil {
			t.Fatal("an unknown flag was accepted")
		}
	})
}

func TestDestructiveCommandPreviewsUntilApplied(t *testing.T) {
	cli := newCLIRig(t)
	out, err := cli.run(t, "stub", "delete", "alpha")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if !strings.Contains(out, "subject: alpha") || !strings.Contains(out, "cannot be undone") {
		t.Fatalf("preview output = %q", out)
	}
	if n, err := cli.rig.Stubs.Ledger(t.Context(), operationtest.Delete); err != nil || n != 0 {
		t.Fatalf("ledger after a preview = %d, %v, want 0", n, err)
	}
	if out, err = cli.run(t, "stub", "delete", "alpha", "--apply"); err != nil || !strings.Contains(out, "done: true") {
		t.Fatalf("apply output = %q, err = %v", out, err)
	}
	if n, err := cli.rig.Stubs.Ledger(t.Context(), operationtest.Delete); err != nil || n != 1 {
		t.Fatalf("ledger after the apply = %d, %v, want 1", n, err)
	}
}

func TestTrustGrantCommandRunsDirectly(t *testing.T) {
	cli := newCLIRig(t)
	if _, err := cli.run(t, "stub", "grant", "alpha"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if n, err := cli.rig.Stubs.Ledger(t.Context(), operationtest.Grant); err != nil || n != 1 {
		t.Fatalf("ledger = %d, %v, want 1", n, err)
	}
	if cli.rig.Stubs.Previews() != 0 {
		t.Fatalf("a trust grant on the CLI ran its previewer %d times", cli.rig.Stubs.Previews())
	}
}
