package agent

import (
	"slices"
	"testing"

	"github.com/anggasct/aura/internal/config"
)

func mcpPersona(tools ...string) config.AgentDefinition {
	return config.AgentDefinition{ID: "ops", Description: "Ops assistant", Instructions: "Help.", Tools: tools}
}

func TestBuildWithMCPAcceptsServerAndToolEntries(t *testing.T) {
	override := mcpPersona("read_file", "mcp_docs", "mcp_files_read_text", "mcp_files_with_underscores_ls")
	registry, err := BuildWithMCP([]config.AgentDefinition{override}, validTools, []string{"docs", "files", "files_with_underscores"}, []string{"primary"})
	if err != nil {
		t.Fatalf("BuildWithMCP: %v", err)
	}
	ops, ok := registry.Lookup("ops")
	if !ok || !slices.Equal(ops.Tools, override.Tools) {
		t.Fatalf("ops tools = %v", ops.Tools)
	}
}

func TestBuildWithMCPRejectsUnknownServers(t *testing.T) {
	servers := []string{"docs"}
	cases := map[string][]string{
		"unknown server entry":     {"mcp_ghost"},
		"unknown server tool":      {"mcp_ghost_read"},
		"bare prefix":              {"mcp_"},
		"server prefix only":       {"mcp_docs_"},
		"prefix of another server": {"mcp_doc"},
		"entry for a longer name":  {"mcp_docsx"},
	}
	for name, tools := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := BuildWithMCP([]config.AgentDefinition{mcpPersona(tools...)}, validTools, servers, []string{"primary"})
			if code, ok := CodeOf(err); !ok || code != ErrorCodePersonaInvalid {
				t.Fatalf("BuildWithMCP() code = %q, %v (err %v), want persona_invalid", code, ok, err)
			}
		})
	}
}

func TestBuildWithoutServersRejectsMCPEntries(t *testing.T) {
	_, err := Build([]config.AgentDefinition{mcpPersona("mcp_docs")}, validTools, []string{"primary"})
	if code, ok := CodeOf(err); !ok || code != ErrorCodePersonaInvalid {
		t.Fatalf("Build() code = %q, %v (err %v), want persona_invalid", code, ok, err)
	}
}

func TestBuildWithMCPKeepsMCPToolsOwnerOnly(t *testing.T) {
	cases := map[string]func(d *config.AgentDefinition){
		"principal server entry": func(d *config.AgentDefinition) { d.PrincipalTools = []string{"mcp_docs"} },
		"principal tool entry":   func(d *config.AgentDefinition) { d.PrincipalTools = []string{"mcp_docs_search"} },
		"public tool entry":      func(d *config.AgentDefinition) { d.PublicTools = []string{"mcp_docs_search"} },
		"public server entry":    func(d *config.AgentDefinition) { d.PublicTools = []string{"mcp_docs"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			override := kantorOverride()
			override.Tools = append(override.Tools, "mcp_docs", "mcp_docs_search")
			mutate(&override)
			_, err := BuildWithMCP([]config.AgentDefinition{override}, validTools, []string{"docs"}, []string{"primary"})
			if code, ok := CodeOf(err); !ok || code != ErrorCodePersonaInvalid {
				t.Fatalf("BuildWithMCP() code = %q, %v (err %v), want persona_invalid", code, ok, err)
			}
		})
	}
}

func TestBuildWithMCPLeavesPersonasWithoutEntriesUntouched(t *testing.T) {
	plain, err := Build([]config.AgentDefinition{kantorOverride()}, validTools, []string{"primary"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	withServers, err := BuildWithMCP([]config.AgentDefinition{kantorOverride()}, validTools, []string{"docs"}, []string{"primary"})
	if err != nil {
		t.Fatalf("BuildWithMCP: %v", err)
	}
	for _, id := range []string{"main", "engineer", "reviewer", "researcher", "kantor"} {
		a, _ := plain.Lookup(id)
		b, _ := withServers.Lookup(id)
		if a.Fingerprint() != b.Fingerprint() || !slices.Equal(a.Tools, b.Tools) {
			t.Errorf("persona %s changed when an mcp server was configured", id)
		}
	}
}
