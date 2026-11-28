package operation_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
)

func shapeCatalog() operation.Catalog {
	read := func(id string, output operation.Output) operation.Descriptor {
		return operation.Descriptor{
			ID: id, Summary: "show a shape", Exposure: operation.ExposureOwner, Risk: operation.RiskRead,
			Commit: operation.CommitNone, UntrustedOutput: true, Output: output,
		}
	}
	return operation.Catalog{
		Group: operation.Group{Name: "shape", Summary: "output shapes"},
		Descriptors: []operation.Descriptor{
			read("shape.table", operation.Output{
				Kind:   operation.OutputTable,
				Header: []operation.OutputField{{Name: "name"}, {Name: "total"}},
				Fields: []operation.OutputField{{Name: "name"}, {Name: "size"}},
			}),
			read("shape.record", operation.Output{Kind: operation.OutputRecord, Fields: []operation.OutputField{{Name: "name"}, {Name: "items"}, {Name: "none"}}}),
		},
	}
}

func shapeBindings(*operationtest.Stubs) []operation.Binding {
	data := func(object map[string]any) operation.Handler {
		return func(context.Context, *operation.Call) (operation.Result, error) { return operation.JSONResult(object) }
	}
	return []operation.Binding{
		{ID: "shape.table", Handle: data(map[string]any{"name": "docs", "total": 2, "rows": []map[string]any{{"name": "a", "size": 1}, {"name": "b", "size": 2}}})},
		{ID: "shape.record", Handle: data(map[string]any{"name": "docs", "items": []string{"first ‮ item", "second"}, "none": []string{}})},
	}
}

func shapeRig(t *testing.T) (*operationtest.Rig, *cobra.Command) {
	t.Helper()
	rig := operationtest.NewRig(t, &operationtest.Options{Catalogs: []operation.Catalog{shapeCatalog()}, Bindings: shapeBindings, ChatCommands: true})
	open := func(_ *cobra.Command, _ *operation.Descriptor, run func(context.Context, operation.Invoker) error) error {
		return run(t.Context(), rig.Registry)
	}
	commands, err := operation.Commands([]operation.Catalog{shapeCatalog()}, open)
	if err != nil {
		t.Fatalf("Commands: %v", err)
	}
	root := &cobra.Command{Use: "aura", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(commands...)
	return rig, root
}

func TestATableOutputPrintsItsHeaderRecordAboveTheRows(t *testing.T) {
	rig, root := shapeRig(t)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"shape", "table"})
	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("shape table: %v", err)
	}
	want := "name: docs\ntotal: 2\nNAME\tSIZE\na\t1\nb\t2\n"
	if out.String() != want {
		t.Errorf("CLI = %q, want %q", out.String(), want)
	}

	chat, err := operation.NewChat(&operation.ChatDeps{Registry: rig.Registry, Capabilities: capabilitytest.Host(t)})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	plan := chat.Plan("shape", tokens(t, "table"))
	owner := &command.Caller{Channel: "telegram", PrincipalID: operationtest.OwnerID, Class: principal.ClassOwner, SessionID: operationtest.SessionID, AgentID: "main", Direct: true}
	reply := chat.Run(t.Context(), &command.OperationRun{Caller: owner, Plan: &plan, IdempotencyKey: "telegram:shape"})
	if got := strings.Join(reply.Lines, "\n") + "\n"; got != want {
		t.Errorf("chat = %q, want %q", got, want)
	}
}

func TestARecordOutputPrintsEachListItemOnItsOwnLine(t *testing.T) {
	_, root := shapeRig(t)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"shape", "record"})
	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("shape record: %v", err)
	}
	want := "name: docs\nitems:\n  - first \\u{202e} item\n  - second\nnone: -\n"
	if out.String() != want {
		t.Errorf("CLI = %q, want %q", out.String(), want)
	}
}

func TestACatalogRejectsABadOutputHeader(t *testing.T) {
	cases := map[string]func(d *operation.Descriptor){
		"a header on a record": func(d *operation.Descriptor) {
			d.Output = operation.Output{Kind: operation.OutputRecord, Header: []operation.OutputField{{Name: "name"}}}
		},
		"a secret header field": func(d *operation.Descriptor) { d.Output.Header = []operation.OutputField{{Name: "name", Secret: true}} },
		"a repeated header field": func(d *operation.Descriptor) {
			d.Output.Header = []operation.OutputField{{Name: "name"}, {Name: "name"}}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			catalog := shapeCatalog()
			change(&catalog.Descriptors[0])
			err := operation.ValidateCatalogs([]operation.Catalog{catalog})
			if !fault.HasCode(err, operation.ErrorCodeInvalid) {
				t.Fatalf("ValidateCatalogs = %v, want operation_invalid", err)
			}
		})
	}
}
