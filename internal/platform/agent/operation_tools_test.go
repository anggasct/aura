package agent

import (
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	auratools "github.com/anggasct/aura/internal/platform/tools"
)

const operationTool = "aura_cron_list"

var toolsWithAnOperation = append(slices.Clone(validTools), operationTool)

var catalogWithAnOperation = append(slices.Clone(validToolCatalog), auratools.Definition{Name: operationTool, Version: auratools.Version})

func TestPersonaOperationConfigValidation(t *testing.T) {
	cases := []struct {
		name      string
		persona   Persona
		wantField string
	}{
		{name: "a trusted persona listing an operation tool", persona: Persona{ID: "desk", Audience: AudienceTrusted, Tools: []string{"read_file", operationTool}}, wantField: "definitions[0].tools"},
		{name: "a public persona listing an operation tool", persona: Persona{ID: "desk", Audience: AudiencePublic, Tools: []string{operationTool}}, wantField: "definitions[0].tools"},
		{name: "an owner persona with an operation tool in principal_tools", persona: Persona{ID: "desk", Tools: []string{"read_file", operationTool}, PrincipalTools: []string{operationTool}}, wantField: "definitions[0].principal_tools"},
		{name: "an owner persona with an operation tool in public_tools", persona: Persona{ID: "desk", Tools: []string{"read_file", operationTool}, PublicTools: []string{operationTool}}, wantField: "definitions[0].public_tools"},
		{name: "a trusted persona with an operation tool in principal_tools", persona: Persona{ID: "desk", Audience: AudienceTrusted, PrincipalTools: []string{operationTool}}, wantField: "definitions[0].principal_tools"},
	}
	for _, tc := range cases {
		t.Run(tc.name+" fails at load", func(t *testing.T) {
			err := Config{Definitions: []Persona{tc.persona}}.Validate()
			if !fault.HasCode(err, ErrorCodePersonaInvalid) {
				t.Fatalf("Validate = %v, want persona_invalid", err)
			}
			if !strings.Contains(err.Error(), tc.wantField) || strings.Contains(err.Error(), operationTool) {
				t.Fatalf("Validate = %q, want the path %s and no tool name", err, tc.wantField)
			}
		})
		t.Run(tc.name+" fails at build", func(t *testing.T) {
			_, err := Build([]Persona{tc.persona}, toolsWithAnOperation, catalogWithAnOperation, nil, "en")
			if !fault.HasCode(err, ErrorCodePersonaInvalid) {
				t.Fatalf("Build = %v, want persona_invalid", err)
			}
		})
	}
	t.Run("an owner persona may list the discovery tool and a generated operation tool", func(t *testing.T) {
		persona := Persona{ID: "desk", Description: "d", Instructions: "x", Tools: []string{"read_file", operationTool, auratools.OperationSearchToolName}}
		if err := (Config{Definitions: []Persona{persona}}).Validate(); err != nil {
			t.Fatalf("Validate = %v", err)
		}
		registry, err := Build([]Persona{persona}, toolsWithAnOperation, catalogWithAnOperation, nil, "en")
		if err != nil {
			t.Fatalf("Build = %v", err)
		}
		desk, _ := registry.Lookup("desk")
		for _, want := range []string{operationTool, auratools.OperationSearchToolName} {
			if !slices.Contains(desk.Tools, want) {
				t.Fatalf("tools = %v, want %s", desk.Tools, want)
			}
		}
	})
	t.Run("an owner persona listing an aura_ name that is not generated fails at build", func(t *testing.T) {
		persona := Persona{ID: "desk", Description: "d", Instructions: "x", Tools: []string{"read_file", "aura_cron_everything"}}
		if err := (Config{Definitions: []Persona{persona}}).Validate(); err != nil {
			t.Fatalf("Validate = %v", err)
		}
		_, err := Build([]Persona{persona}, toolsWithAnOperation, catalogWithAnOperation, nil, "en")
		if !fault.HasCode(err, ErrorCodePersonaInvalid) {
			t.Fatalf("Build = %v, want persona_invalid", err)
		}
		if !strings.Contains(err.Error(), "agents.definitions[0].tools") || strings.Contains(err.Error(), "aura_cron_everything") {
			t.Fatalf("Build = %q, want the path and no tool name", err)
		}
	})
	t.Run("an ordinary unknown tool is still an unknown tool", func(t *testing.T) {
		persona := Persona{ID: "desk", Description: "d", Instructions: "x", Tools: []string{"read_file", "no_such_tool"}}
		_, err := Build([]Persona{persona}, toolsWithAnOperation, catalogWithAnOperation, nil, "en")
		if !fault.HasCode(err, ErrorCodeUnknownTool) {
			t.Fatalf("Build = %v, want agent_unknown_tool", err)
		}
	})
}

func TestPersonaWithoutToolListGetsNoOperations(t *testing.T) {
	cases := []struct {
		name     string
		audience string
	}{
		{name: "owner", audience: AudienceOwner},
		{name: "trusted", audience: AudienceTrusted},
		{name: "public", audience: AudiencePublic},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registry, err := Build([]Persona{{ID: "desk", Description: "d", Instructions: "x", Audience: tc.audience}}, toolsWithAnOperation, catalogWithAnOperation, nil, "en")
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			desk, _ := registry.Lookup("desk")
			if slices.ContainsFunc(desk.Tools, auratools.IsOperationEntry) {
				t.Fatalf("tools = %v, want no operation tool", desk.Tools)
			}
		})
	}
}

func TestBuiltinOverrideWithNonOwnerAudienceDropsOperationTools(t *testing.T) {
	inherited := []string{"read_file", operationTool, auratools.OperationSearchToolName}
	cases := []struct {
		name     string
		audience string
		want     []string
	}{
		{name: "owner keeps them", audience: AudienceOwner, want: inherited},
		{name: "trusted drops them", audience: AudienceTrusted, want: []string{"read_file"}},
		{name: "public drops them", audience: AudiencePublic, want: []string{"read_file"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := inheritedTools(inherited, tc.audience); !slices.Equal(got, tc.want) {
				t.Fatalf("inheritedTools = %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("an override of main with audience trusted and no tool list loads", func(t *testing.T) {
		registry, err := Build([]Persona{{ID: DefaultID, Audience: AudienceTrusted}}, toolsWithAnOperation, catalogWithAnOperation, nil, "en")
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		main, _ := registry.Lookup(DefaultID)
		if slices.ContainsFunc(main.Tools, auratools.IsOperationEntry) {
			t.Fatalf("tools = %v, want no operation tool", main.Tools)
		}
	})
}

func TestLiveMainShapeStillLoads(t *testing.T) {
	workflowTools := []string{"workflow_list", "workflow_start", "workflow_status", "workflow_cancel", "workflow_propose", "workflow_save_routine"}
	known := append(slices.Clone(validTools), workflowTools...)
	catalog := slices.Clone(validToolCatalog)
	for _, name := range workflowTools {
		catalog = append(catalog, auratools.Definition{Name: name, Version: auratools.Version})
	}
	main := Persona{ID: DefaultID, Tools: append(slices.Clone(validTools), workflowTools...)}
	if err := (Config{Definitions: []Persona{main}}).Validate(); err != nil {
		t.Fatalf("Validate = %v", err)
	}
	registry, err := Build([]Persona{main}, known, catalog, nil, "en")
	if err != nil {
		t.Fatalf("Build = %v", err)
	}
	got, _ := registry.Lookup(DefaultID)
	if len(got.Tools) != len(known) || got.Audience != AudienceOwner {
		t.Fatalf("main = %d tools, audience %q; want %d tools, owner", len(got.Tools), got.Audience, len(known))
	}
}

func TestBuiltinMainPersonaEnablesOperations(t *testing.T) {
	registry, err := Build(nil, toolsWithAnOperation, catalogWithAnOperation, nil, "en")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	main, ok := registry.Lookup(DefaultID)
	if !ok || !slices.Contains(main.Tools, auratools.OperationSearchToolName) {
		t.Fatalf("main = %+v, want it to list the discovery tool", main)
	}
	if main.Audience != AudienceOwner {
		t.Fatalf("main audience = %q, want owner", main.Audience)
	}
	for _, id := range []string{"engineer", "reviewer", "researcher"} {
		definition, _ := registry.Lookup(id)
		if slices.ContainsFunc(definition.Tools, auratools.IsOperationEntry) {
			t.Errorf("%s lists %v, want no operation tool", id, definition.Tools)
		}
	}
	t.Run("an override of main without a tool list keeps the discovery tool", func(t *testing.T) {
		registry, err := Build([]Persona{{ID: DefaultID, Instructions: "be brief"}}, toolsWithAnOperation, catalogWithAnOperation, nil, "en")
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		main, _ := registry.Lookup(DefaultID)
		if !slices.Contains(main.Tools, auratools.OperationSearchToolName) {
			t.Fatalf("tools = %v, want the discovery tool", main.Tools)
		}
	})
}
