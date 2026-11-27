package runtimeadk

import (
	"slices"
	"testing"

	"google.golang.org/adk/v2/tool"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
)

func TestToolsForHonoursTheRestriction(t *testing.T) {
	executor := &ADKExecutor{
		tools: []tool.Tool{
			namedToolStub{name: "read_file"}, namedToolStub{name: "list_dir"}, namedToolStub{name: "web_fetch"},
			namedToolStub{name: "exec"}, namedToolStub{name: "workflow_list"},
		},
		optIn:      map[string]bool{"workflow_list": true},
		ownerOptIn: map[string]bool{"workflow_list": true},
	}
	listed := &auraagent.Definition{Tools: []string{"read_file", "list_dir", "workflow_list"}}
	implicit := &auraagent.Definition{}
	cases := []struct {
		name        string
		definition  *auraagent.Definition
		origin      turn.Origin
		restriction *turn.ToolRestriction
		want        []string
	}{
		{name: "nil restriction keeps the persona list", definition: listed, origin: "discord:main", want: []string{"read_file", "list_dir", "workflow_list"}},
		{name: "nil restriction keeps the implicit list", definition: implicit, origin: "discord:main", want: []string{"read_file", "list_dir", "web_fetch", "exec"}},
		{name: "empty restriction offers no tool", definition: listed, origin: "discord:main", restriction: &turn.ToolRestriction{}, want: []string{}},
		{name: "empty allowed list offers no tool", definition: implicit, origin: "discord:main", restriction: &turn.ToolRestriction{Allowed: []string{}}, want: []string{}},
		{name: "listed restriction offers only the listed names", definition: implicit, origin: "discord:main", restriction: &turn.ToolRestriction{Allowed: []string{"read_file", "list_dir"}}, want: []string{"read_file", "list_dir"}},
		{name: "restriction never adds a tool the persona does not list", definition: listed, origin: "discord:main", restriction: &turn.ToolRestriction{Allowed: []string{"read_file", "web_fetch"}}, want: []string{"read_file"}},
		{name: "restriction never adds an opt-in tool the persona does not list", definition: implicit, origin: "discord:main", restriction: &turn.ToolRestriction{Allowed: []string{"workflow_list"}}, want: []string{}},
		{name: "owner on a channel gets the listed owner opt-in tool", definition: listed, origin: "discord:main", restriction: &turn.ToolRestriction{Allowed: []string{"read_file", "workflow_list"}}, want: []string{"read_file", "workflow_list"}},
		{name: "workflow origin never gets an owner opt-in tool even when listed", definition: listed, origin: turn.OriginWorkflow, restriction: &turn.ToolRestriction{Allowed: []string{"read_file", "workflow_list"}}, want: []string{"read_file"}},
		{name: "workflow origin without a restriction never gets an owner opt-in tool", definition: listed, origin: turn.OriginWorkflow, want: []string{"read_file", "list_dir"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toolNames(executor.toolsFor(tc.definition, principal.ClassOwner, tc.origin, turnContext{restriction: tc.restriction}))
			if got == nil {
				got = []string{}
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("tools = %v, want %v", got, tc.want)
			}
		})
	}
}
