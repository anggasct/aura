package agent

import (
	"slices"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
)

func mcpPersona(tools ...string) Persona {
	return Persona{ID: "ops", Description: "Ops assistant", Instructions: "Help.", Tools: tools}
}

func TestBuildWithMCPAcceptsServerAndToolEntries(t *testing.T) {
	override := mcpPersona("read_file", "mcp_docs", "mcp_files_read_text", "mcp_files_with_underscores_ls")
	registry, err := BuildWithMCP([]Persona{override}, validTools, validToolCatalog, []string{"docs", "files", "files_with_underscores"}, []string{"primary"})
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
			_, err := BuildWithMCP([]Persona{mcpPersona(tools...)}, validTools, validToolCatalog, servers, []string{"primary"})
			if !fault.HasCode(err, ErrorCodePersonaInvalid) {
				t.Fatalf("BuildWithMCP() error = %v, want persona_invalid", err)
			}
		})
	}
}

func TestBuildWithoutServersRejectsMCPEntries(t *testing.T) {
	_, err := Build([]Persona{mcpPersona("mcp_docs")}, validTools, validToolCatalog, []string{"primary"})
	if !fault.HasCode(err, ErrorCodePersonaInvalid) {
		t.Fatalf("Build() error = %v, want persona_invalid", err)
	}
}

func mcpTierPersona(tools, principals []string) Persona {
	override := kantorOverride()
	override.Tools = append(override.Tools, tools...)
	override.PrincipalTools = append(override.PrincipalTools, principals...)
	return override
}

func TestBuildWithMCPAcceptsPrincipalEntriesCoveredByTools(t *testing.T) {
	cases := map[string]Persona{
		"server entry covered by server entry":   mcpTierPersona([]string{"mcp_docs"}, []string{"mcp_docs"}),
		"tool entry covered by tool entry":       mcpTierPersona([]string{"mcp_docs_search"}, []string{"mcp_docs_search"}),
		"tool entry covered by its server entry": mcpTierPersona([]string{"mcp_docs"}, []string{"mcp_docs_search"}),
	}
	for name, override := range cases {
		t.Run(name, func(t *testing.T) {
			registry, err := BuildWithMCP([]Persona{override}, validTools, validToolCatalog, []string{"docs"}, []string{"primary"})
			if err != nil {
				t.Fatalf("BuildWithMCP: %v", err)
			}
			definition, _ := registry.Lookup("kantor")
			ceiling, restricted := definition.ToolCeiling(principal.ClassTrusted)
			if !restricted || !slices.Equal(ceiling, override.PrincipalTools) {
				t.Fatalf("trusted ceiling = %v, want %v", ceiling, override.PrincipalTools)
			}
		})
	}
}

func TestBuildWithMCPRejectsPrincipalEntriesNotCoveredByTools(t *testing.T) {
	cases := map[string]Persona{
		"no mcp entry in tools":            mcpTierPersona(nil, []string{"mcp_docs_search"}),
		"server entry over tool entry":     mcpTierPersona([]string{"mcp_docs_search"}, []string{"mcp_docs"}),
		"sibling tool entry":               mcpTierPersona([]string{"mcp_docs_read"}, []string{"mcp_docs_search"}),
		"tool entry longer than tools one": mcpTierPersona([]string{"mcp_docs_search"}, []string{"mcp_docs_search_all"}),
		"other server":                     mcpTierPersona([]string{"mcp_docs"}, []string{"mcp_files_read"}),
	}
	for name, override := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := BuildWithMCP([]Persona{override}, validTools, validToolCatalog, []string{"docs", "files"}, []string{"primary"})
			if !fault.HasCode(err, ErrorCodePersonaInvalid) {
				t.Fatalf("BuildWithMCP() error = %v, want persona_invalid", err)
			}
		})
	}
}

func TestBuildWithMCPRejectsPublicEntries(t *testing.T) {
	for _, entry := range []string{"mcp_docs", "mcp_docs_search"} {
		t.Run(entry, func(t *testing.T) {
			override := kantorOverride()
			override.Tools = append(override.Tools, "mcp_docs", "mcp_docs_search")
			override.PublicTools = []string{entry}
			_, err := BuildWithMCP([]Persona{override}, validTools, validToolCatalog, []string{"docs"}, []string{"primary"})
			if !fault.HasCode(err, ErrorCodePersonaInvalid) {
				t.Fatalf("BuildWithMCP() error = %v, want persona_invalid", err)
			}
		})
	}
}

func TestMCPEntriesChangeThePersonaFingerprintAndEvalDigest(t *testing.T) {
	build := func(override Persona) Definition {
		registry, err := BuildWithMCP([]Persona{override}, validTools, validToolCatalog, []string{"docs"}, []string{"primary"})
		if err != nil {
			t.Fatalf("BuildWithMCP: %v", err)
		}
		definition, _ := registry.Lookup("kantor")
		return definition
	}
	base := build(kantorOverride())
	withTools := build(mcpTierPersona([]string{"mcp_docs"}, nil))
	withPrincipal := build(mcpTierPersona([]string{"mcp_docs"}, []string{"mcp_docs"}))
	narrowed := build(mcpTierPersona([]string{"mcp_docs"}, []string{"mcp_docs_search"}))
	if withTools.Fingerprint().ToolsSHA256 == base.Fingerprint().ToolsSHA256 {
		t.Error("an mcp entry in tools left the tools fingerprint unchanged")
	}
	digests := map[string]string{}
	for name, definition := range map[string]Definition{"base": base, "tools": withTools, "principal": withPrincipal, "narrowed": narrowed} {
		digest := definition.EvalDigest()
		if other, clash := digests[digest]; clash {
			t.Errorf("%s and %s share an eval digest", name, other)
		}
		digests[digest] = name
	}
}

func TestBuildWithMCPLeavesPersonasWithoutEntriesUntouched(t *testing.T) {
	plain, err := Build([]Persona{kantorOverride()}, validTools, validToolCatalog, []string{"primary"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	withServers, err := BuildWithMCP([]Persona{kantorOverride()}, validTools, validToolCatalog, []string{"docs"}, []string{"primary"})
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
