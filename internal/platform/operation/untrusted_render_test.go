package operation_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
)

const hostileText = "red\u001b[31m \u202egnp.exe\a end"

var hostileMarks = []string{"\u001b", "\u202e", "\a"}

func hostileCatalog() operation.Catalog {
	read := func(id string, untrusted bool, output operation.Output) operation.Descriptor {
		return operation.Descriptor{
			ID: id, Summary: "show hostile text", Exposure: operation.ExposureOwner, Risk: operation.RiskRead,
			Commit: operation.CommitNone, UntrustedOutput: untrusted, Output: output,
		}
	}
	deletion := operation.Descriptor{
		ID: "hostile.delete", Summary: "delete hostile text", Exposure: operation.ExposureOwner, Risk: operation.RiskDestructive,
		Commit: operation.CommitTransactional, Preview: true,
		Params: []operation.Param{{Name: "name", Kind: operation.KindString, Summary: "name", Required: true, Position: 1, MinLength: 1, MaxLength: 16}},
		Output: operation.Output{Kind: operation.OutputText},
	}
	return operation.Catalog{
		Group: operation.Group{Name: "hostile", Summary: "hostile output"},
		Descriptors: []operation.Descriptor{
			read("hostile.table", true, operation.Output{Kind: operation.OutputTable, Fields: []operation.OutputField{{Name: "a"}, {Name: "b"}}}),
			read("hostile.record", true, operation.Output{Kind: operation.OutputRecord, Fields: []operation.OutputField{{Name: "a"}, {Name: "b"}}}),
			read("hostile.text", true, operation.Output{Kind: operation.OutputText}),
			read("hostile.plain", false, operation.Output{Kind: operation.OutputText}),
			deletion,
		},
	}
}

func hostileBindings(*operationtest.Stubs) []operation.Binding {
	data := func(object map[string]any) operation.Handler {
		return func(context.Context, *operation.Call) (operation.Result, error) { return operation.JSONResult(object) }
	}
	return []operation.Binding{
		{ID: "hostile.table", Handle: data(map[string]any{"rows": []map[string]any{{"a": hostileText, "b": "fine"}}})},
		{ID: "hostile.record", Handle: data(map[string]any{"a": hostileText, "b": "fine"})},
		{ID: "hostile.text", Handle: data(map[string]any{"text": "first " + hostileText + "\nsecond line"})},
		{ID: "hostile.plain", Handle: data(map[string]any{"text": "plain line"})},
		{
			ID: "hostile.delete",
			Handle: func(ctx context.Context, call *operation.Call) (operation.Result, error) {
				return call.Commit(ctx, func(context.Context) (operation.Result, error) {
					return operation.JSONResult(map[string]any{"text": "deleted"})
				})
			},
			Preview: func(context.Context, *operation.Call) (operation.Preview, error) {
				return operation.Preview{Lines: []string{"would delete " + hostileText}, Digest: "digest"}, nil
			},
		},
	}
}

func hostileRig(t *testing.T) (*operationtest.Rig, *cobra.Command) {
	t.Helper()
	rig := operationtest.NewRig(t, &operationtest.Options{
		Catalogs: []operation.Catalog{hostileCatalog()}, Bindings: hostileBindings, ChatCommands: true,
	})
	open := func(_ *cobra.Command, _ *operation.Descriptor, run func(context.Context, operation.Invoker) error) error {
		return run(t.Context(), rig.Registry)
	}
	commands, err := operation.Commands([]operation.Catalog{hostileCatalog()}, open)
	if err != nil {
		t.Fatalf("Commands: %v", err)
	}
	root := &cobra.Command{Use: "aura", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(commands...)
	return rig, root
}

func requireNoRawMarks(t *testing.T, where, text string) {
	t.Helper()
	for _, raw := range hostileMarks {
		if strings.Contains(text, raw) {
			t.Errorf("%s printed a raw %q:\n%s", where, raw, text)
		}
	}
	if !strings.Contains(text, `\u{202e}`) {
		t.Errorf("%s did not show the escaped direction control:\n%s", where, text)
	}
}

func TestUntrustedOutputIsEscapedOnTheCLIForEveryOutputKind(t *testing.T) {
	_, root := hostileRig(t)
	for _, verb := range []string{"table", "record", "text"} {
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs([]string{"hostile", verb})
		if err := root.ExecuteContext(t.Context()); err != nil {
			t.Fatalf("hostile %s: %v", verb, err)
		}
		requireNoRawMarks(t, "hostile "+verb, out.String())
	}
}

func TestTextOutputKeepsItsLineBreaksWhileEscaped(t *testing.T) {
	_, root := hostileRig(t)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"hostile", "text"})

	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("hostile text: %v", err)
	}

	if lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n"); len(lines) != 2 || lines[1] != "second line" {
		t.Errorf("lines = %q, want the two lines kept", lines)
	}
}

func TestOutputThatIsNotFlaggedUntrustedPrintsAsItIs(t *testing.T) {
	_, root := hostileRig(t)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"hostile", "plain"})

	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("hostile plain: %v", err)
	}

	if strings.TrimSpace(out.String()) != "plain line" {
		t.Errorf("out = %q", out.String())
	}
}

func TestUntrustedOutputIsEscapedInTheJSONSpelling(t *testing.T) {
	_, root := hostileRig(t)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"hostile", "record", "--json"})

	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("hostile record --json: %v", err)
	}

	for _, raw := range hostileMarks {
		if strings.Contains(out.String(), raw) {
			t.Errorf("--json printed a raw %q:\n%s", raw, out.String())
		}
	}
	var decoded map[string]string
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil || decoded["a"] != hostileText {
		t.Errorf("--json = %s (%v), want the value to decode back to the original", out.String(), err)
	}
}

func TestPreviewLinesAreEscapedOnTheCLI(t *testing.T) {
	_, root := hostileRig(t)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"hostile", "delete", "thing"})

	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("hostile delete: %v", err)
	}

	requireNoRawMarks(t, "the delete preview", out.String())
}

func TestUntrustedOutputAndPreviewLinesAreEscapedInChat(t *testing.T) {
	rig, _ := hostileRig(t)
	chat, err := operation.NewChat(&operation.ChatDeps{Registry: rig.Registry, Capabilities: capabilitytest.Host(t)})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	owner := &command.Caller{Channel: "telegram", PrincipalID: operationtest.OwnerID, Class: principal.ClassOwner, SessionID: operationtest.SessionID, AgentID: "main", Direct: true}
	for _, message := range []string{"table", "record", "text", "delete thing"} {
		plan := chat.Plan("hostile", tokens(t, message))
		if plan.Refusal != nil || plan.Missing != nil {
			t.Fatalf("Plan(%q) = %+v", message, plan)
		}
		reply := chat.Run(t.Context(), &command.OperationRun{Caller: owner, Plan: &plan, IdempotencyKey: "telegram:" + message})
		requireNoRawMarks(t, "chat "+message, strings.Join(reply.Lines, "\n"))
	}
}
