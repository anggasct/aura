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
	t.Run("an owner persona listing an operation tool is refused as an unknown tool while none is offered", func(t *testing.T) {
		persona := Persona{ID: "desk", Description: "d", Instructions: "x", Tools: []string{"read_file", operationTool}}
		if err := (Config{Definitions: []Persona{persona}}).Validate(); err != nil {
			t.Fatalf("Validate = %v", err)
		}
		_, err := Build([]Persona{persona}, validTools, validToolCatalog, nil, "en")
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
	inherited := []string{"read_file", operationTool, "aura_operation_search"}
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
