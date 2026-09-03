package agent

import (
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/config"
)

var validTools = []string{"read_file", "write_file", "list_dir", "exec", "web_fetch", "web_search"}

func TestBuildRegistersBuiltins(t *testing.T) {
	registry, err := Build(nil, validTools, []string{"primary", "auxiliary"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	definitions := registry.Definitions()
	wantIDs := []string{"main", "engineer", "reviewer", "researcher"}
	if len(definitions) != len(wantIDs) {
		t.Fatalf("registered definitions = %d, want %d", len(definitions), len(wantIDs))
	}
	for i, definition := range definitions {
		if definition.ID != wantIDs[i] {
			t.Errorf("definitions[%d] = %q, want %q", i, definition.ID, wantIDs[i])
		}
	}
	main, ok := registry.Lookup(DefaultID)
	if !ok {
		t.Fatal("default agent is not registered")
	}
	if len(main.Capabilities) != 0 {
		t.Errorf("main declares capabilities %v, want none", main.Capabilities)
	}
	if len(main.Tools) != len(validTools) {
		t.Errorf("main declares %d tools, want the full builtin set", len(main.Tools))
	}
}

func TestBuildRejectsInvalidOverrides(t *testing.T) {
	cases := []struct {
		name      string
		override  config.AgentDefinition
		wantCode  ErrorCode
		wantField string
	}{
		{
			name:      "empty id",
			override:  config.AgentDefinition{Instructions: "x", Description: "d"},
			wantCode:  ErrorCodeDefinitionInvalid,
			wantField: ".id",
		},
		{
			name:      "unknown tool",
			override:  config.AgentDefinition{ID: "ops", Description: "d", Instructions: "x", Tools: []string{"git_push"}},
			wantCode:  ErrorCodeUnknownTool,
			wantField: ".tools",
		},
		{
			name:      "unknown capability",
			override:  config.AgentDefinition{ID: "ops", Description: "d", Instructions: "x", Capabilities: []string{"kernel.write"}},
			wantCode:  ErrorCodeUnknownCapability,
			wantField: ".capabilities",
		},
		{
			name:      "unknown model route",
			override:  config.AgentDefinition{ID: "ops", Description: "d", Instructions: "x", ModelRoute: "tertiary"},
			wantCode:  ErrorCodeUnknownModelRoute,
			wantField: ".model_route",
		},
		{
			name:      "empty instructions on a new definition",
			override:  config.AgentDefinition{ID: "ops", Description: "d"},
			wantCode:  ErrorCodeDefinitionInvalid,
			wantField: ".instructions",
		},
		{
			name:      "negative turn timeout",
			override:  config.AgentDefinition{ID: "ops", Description: "d", Instructions: "x", Limits: config.AgentLimits{TurnTimeout: -1}},
			wantCode:  ErrorCodeDefinitionInvalid,
			wantField: ".limits.turn_timeout",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registry, err := Build([]config.AgentDefinition{tc.override}, validTools, []string{"primary", "auxiliary"})
			if err == nil {
				t.Fatal("Build accepted an invalid definition")
			}
			code, ok := CodeOf(err)
			if !ok || code != tc.wantCode {
				t.Fatalf("code = %s (%v), want %s", code, err, tc.wantCode)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("error %v does not name the offending field %q", err, tc.wantField)
			}
			if registry != nil {
				t.Error("Build returned a registry alongside an error")
			}
		})
	}
}

func TestResolveExplicitID(t *testing.T) {
	registry, err := Build(nil, validTools, []string{"primary"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	prefer := "reviewer"
	definition, err := registry.Resolve(nil, &prefer)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if definition.ID != "reviewer" {
		t.Fatalf("resolved %q, want reviewer", definition.ID)
	}
	missing := "nonexistent"
	code, ok := CodeOf(registry.mustResolveErr(t, &missing))
	if !ok || code != ErrorCodeNotFound {
		t.Fatalf("code = %s, want agent_not_found", code)
	}
}

func (r *Registry) mustResolveErr(t *testing.T, prefer *string) error {
	t.Helper()
	_, err := r.Resolve(nil, prefer)
	if err == nil {
		t.Fatal("Resolve unexpectedly succeeded")
	}
	return err
}

func TestResolveMostSpecificMatchWins(t *testing.T) {
	registry, err := Build(nil, validTools, []string{"primary"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	cases := []struct {
		name     string
		required []string
		wantID   string
	}{
		{
			name:     "no requirements select the default agent",
			required: nil,
			wantID:   DefaultID,
		},
		{
			name:     "repository work selects the engineer",
			required: []string{CapabilityRepositoryWrite},
			wantID:   "engineer",
		},
		{
			name:     "review work selects the reviewer",
			required: []string{CapabilityCodeReview},
			wantID:   "reviewer",
		},
		{
			name:     "web work selects the researcher",
			required: []string{CapabilityWebSearch, CapabilityWebRead},
			wantID:   "researcher",
		},
		{
			name:     "engineer covers repository read and shell",
			required: []string{CapabilityRepositoryRead, CapabilityShellExecute},
			wantID:   "engineer",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			definition, err := registry.Resolve(tc.required, nil)
			if err != nil {
				t.Fatalf("Resolve(%v): %v", tc.required, err)
			}
			if definition.ID != tc.wantID {
				t.Fatalf("resolved %q, want %q", definition.ID, tc.wantID)
			}
		})
	}
}

func TestResolveFailsClosedWithoutEligibleDefinition(t *testing.T) {
	registry, err := Build(nil, validTools, []string{"primary"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	definition, err := registry.Resolve([]string{CapabilityObservabilityRead}, nil)
	if err == nil {
		t.Fatal("Resolve unexpectedly succeeded without an eligible definition")
	}
	if definition.ID != "" {
		t.Fatalf("returned a partial definition %q on failure", definition.ID)
	}
	code, ok := CodeOf(err)
	if !ok || code != ErrorCodeResolutionFailed {
		t.Fatalf("code = %s, want agent_resolution_failed", code)
	}
}

func TestOverrideReplacesBuiltinWithoutDuplicates(t *testing.T) {
	override := config.AgentDefinition{
		ID:           "engineer",
		Description:  "Custom engineer",
		Instructions: "Custom instructions",
		Capabilities: []string{CapabilityRepositoryRead},
		ModelRoute:   "auxiliary",
	}
	registry, err := Build([]config.AgentDefinition{override}, validTools, []string{"primary", "auxiliary"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	definitions := registry.Definitions()
	if len(definitions) != 4 {
		t.Fatalf("registered definitions = %d, want 4 with the override replacing its builtin", len(definitions))
	}
	engineer, ok := registry.Lookup("engineer")
	if !ok {
		t.Fatal("engineer is not registered after override")
	}
	if engineer.Instructions != "Custom instructions" {
		t.Errorf("instructions = %q, want the override value", engineer.Instructions)
	}
	if engineer.ModelRoute != "auxiliary" {
		t.Errorf("model route = %q, want auxiliary", engineer.ModelRoute)
	}
	if !slices.Equal(engineer.Tools, []string{"read_file", "write_file", "list_dir", "exec"}) {
		t.Errorf("tools = %v, want the retained builtin defaults on omitted field", engineer.Tools)
	}
	ids := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		ids = append(ids, definition.ID)
	}
	if slices.Contains(ids, "") {
		t.Error("registry contains an empty id")
	}
}

func TestBuiltinOverrideKeepsDefaultInstructionsWhenOmitted(t *testing.T) {
	_, builtinErr := builtinByID("main")
	if !builtinErr {
		t.Fatal("main builtin missing")
	}
	override := config.AgentDefinition{ID: "main", Description: "Main override"}
	registry, err := Build([]config.AgentDefinition{override}, validTools, []string{"primary"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	main, ok := registry.Lookup(DefaultID)
	if !ok {
		t.Fatal("main is not registered")
	}
	if main.Instructions == "" {
		t.Error("main override lost the builtin default instructions")
	}
}

func TestResolveTiesBreakByRegistryOrder(t *testing.T) {
	overrideA := config.AgentDefinition{ID: "a-first", Description: "d", Instructions: "x", Capabilities: []string{CapabilityRepositoryRead}}
	overrideB := config.AgentDefinition{ID: "b-second", Description: "d", Instructions: "x", Capabilities: []string{CapabilityRepositoryRead}}
	registry, err := Build([]config.AgentDefinition{overrideA, overrideB}, validTools, []string{"primary"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	definition, err := registry.Resolve([]string{CapabilityRepositoryRead}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if definition.ID != "a-first" {
		t.Fatalf("resolved %q, want the earlier-registered equal-specificity definition", definition.ID)
	}
}
