package knowledge_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
)

func (r *opsRig) cli(t *testing.T, args ...string) string {
	t.Helper()
	open := func(_ *cobra.Command, _ *operation.Descriptor, run func(context.Context, operation.Invoker) error) error {
		return run(t.Context(), r.Registry)
	}
	roots, err := operation.Commands([]operation.Catalog{knowledge.Catalog()}, open)
	if err != nil {
		t.Fatalf("Commands: %v", err)
	}
	var out bytes.Buffer
	root := &cobra.Command{Use: "aura", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(roots...)
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("aura %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

func TestNoControlOrDirectionCharacterOfASourceLabelReachesTheCLI(t *testing.T) {
	rig := newOpsRig(t)
	id := rig.attach(t, operationtest.SessionID, "evil\u202egnp.md\u001b[31m", "text/markdown; charset=utf-8", "# Policy\nRefunds are issued within thirty days.\n")
	if err := requireOK(rig.tool("kb.add", `{"collection":"support","source":"artifact:`+id+`"}`)); err != nil {
		t.Fatalf("add: %v", err)
	}

	out := rig.cli(t, "kb", "show", "support")

	for _, raw := range []string{"\u202e", "\u001b"} {
		if strings.Contains(out, raw) {
			t.Errorf("kb show printed a raw %q:\n%s", raw, out)
		}
	}
}

func TestKnowledgeReadsReturnStructuredResults(t *testing.T) {
	rig := newOpsRig(t)
	seedSource(t, rig)

	for id, wants := range map[string][]string{
		"kb.show":  {`"rows":[{`, `"uri"`, `"digest"`},
		"kb.check": {`"indexed_chunks"`, `"status":"ok"`},
	} {
		input := `{"collection":"` + "support" + `"}`
		if id == "kb.check" {
			input = `{}`
		}
		result, err := rig.invoke(t, operation.SurfaceCLI, id, input)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		for _, want := range wants {
			if !strings.Contains(string(result.Data), want) {
				t.Errorf("%s = %s, want %s", id, result.Data, want)
			}
		}
		if strings.Contains(string(result.Data), `"text"`) {
			t.Errorf("%s still carries a text rendering: %s", id, result.Data)
		}
	}
}
