package agent

import (
	"testing"

	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/tools"
)

func TestStepToolAllowedByClass(t *testing.T) {
	read := tools.Definition{Name: "read_file", Version: tools.Version}
	comment := tools.Definition{Name: "github_comment", Version: tools.Version, OptIn: true}
	remote := tools.Definition{Name: "srv_lookup", Version: tools.Version, Server: "srv"}
	plugged := tools.Definition{Name: "ext.search", Version: tools.Version, Plugin: &tools.PluginRef{ID: "ext"}}
	search := tools.Definition{Name: tools.KBSearchToolName, Version: tools.Version}
	tests := []struct {
		name       string
		definition Definition
		class      principal.Class
		tool       *tools.Definition
		want       bool
	}{
		{name: "owner lists the tool", definition: Definition{Tools: []string{"read_file"}}, class: principal.ClassOwner, tool: &read, want: true},
		{name: "owner does not list the tool", definition: Definition{Tools: []string{"write_file"}}, class: principal.ClassOwner, tool: &read},
		{name: "owner lists an opt-in tool", definition: Definition{Tools: []string{"github_comment"}}, class: principal.ClassOwner, tool: &comment, want: true},
		{name: "owner lists a server's tools", definition: Definition{Tools: []string{tools.MCPServerEntry("srv")}}, class: principal.ClassOwner, tool: &remote, want: true},
		{name: "owner lists a plugin", definition: Definition{Tools: []string{tools.PluginEntry("ext")}}, class: principal.ClassOwner, tool: &plugged, want: true},
		{name: "owner with an empty list gets a plain builtin", definition: Definition{}, class: principal.ClassOwner, tool: &read, want: true},
		{name: "owner with an empty list gets no opt-in tool", definition: Definition{}, class: principal.ClassOwner, tool: &comment},
		{name: "owner with an empty list gets no server tool", definition: Definition{}, class: principal.ClassOwner, tool: &remote},
		{name: "owner with an empty list gets no plugin tool", definition: Definition{}, class: principal.ClassOwner, tool: &plugged},
		{name: "owner searches knowledge without listing it", definition: Definition{Tools: []string{"read_file"}, Knowledge: []string{"docs"}}, class: principal.ClassOwner, tool: &search, want: true},
		{name: "owner lists knowledge search without knowledge", definition: Definition{Tools: []string{tools.KBSearchToolName}}, class: principal.ClassOwner, tool: &search},
		{name: "trusted lists knowledge search with knowledge", definition: Definition{PrincipalTools: []string{tools.KBSearchToolName}, PrincipalKnowledge: []string{"docs"}}, class: principal.ClassTrusted, tool: &search, want: true},
		{name: "trusted has knowledge but does not list the search", definition: Definition{PrincipalTools: []string{"read_file"}, PrincipalKnowledge: []string{"docs"}}, class: principal.ClassTrusted, tool: &search},
		{name: "trusted lists the search but has no knowledge", definition: Definition{PrincipalTools: []string{tools.KBSearchToolName}, Knowledge: []string{"docs"}}, class: principal.ClassTrusted, tool: &search},
		{name: "trusted lists the tool", definition: Definition{PrincipalTools: []string{"read_file"}}, class: principal.ClassTrusted, tool: &read, want: true},
		{name: "trusted does not list the tool", definition: Definition{Tools: []string{"read_file"}, PrincipalTools: []string{"write_file"}}, class: principal.ClassTrusted, tool: &read},
		{name: "trusted with no ceiling gets nothing", definition: Definition{Tools: []string{"read_file"}}, class: principal.ClassTrusted, tool: &read},
		{name: "public lists the tool", definition: Definition{PublicTools: []string{"read_file"}}, class: principal.ClassPublic, tool: &read, want: true},
		{name: "public ceiling is its own list", definition: Definition{PrincipalTools: []string{"read_file"}}, class: principal.ClassPublic, tool: &read},
		{name: "public lists the search with knowledge", definition: Definition{PublicTools: []string{tools.KBSearchToolName}, PublicKnowledge: []string{"docs"}}, class: principal.ClassPublic, tool: &search, want: true},
		{name: "an unknown class gets nothing", definition: Definition{Tools: []string{"read_file"}, Knowledge: []string{"docs"}}, class: principal.Class("stranger"), tool: &read},
		{name: "an unknown class gets no search", definition: Definition{Knowledge: []string{"docs"}}, class: principal.Class("stranger"), tool: &search},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.definition.StepToolAllowed(tc.class, tc.tool); got != tc.want {
				t.Fatalf("StepToolAllowed(%s, %s) = %v, want %v", tc.class, tc.tool.Name, got, tc.want)
			}
		})
	}
}
