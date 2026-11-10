package app

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/anggasct/aura/internal/config"
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	personaeval "github.com/anggasct/aura/internal/modules/eval/persona"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/principal"
)

func TestPersonaPolicySuites(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "modules", "eval", "persona", "testdata", "personas.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Definitions []config.AgentDefinition `yaml:"definitions"`
	}
	if err := yamlv3.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	registry, err := auraagent.Build(file.Definitions, toolsbuiltin.DefinitionNames(), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for i := range file.Definitions {
		id := file.Definitions[i].ID
		t.Run(id, func(t *testing.T) {
			definition, ok := registry.Lookup(id)
			if !ok {
				t.Fatalf("persona %s not registered", id)
			}
			cases, err := personaeval.LoadCases(filepath.Join("..", "modules", "eval", "persona", "testdata", "evals", id))
			if err != nil || len(cases) == 0 {
				t.Fatalf("LoadCases(%s) = %d cases, %v", id, len(cases), err)
			}
			report, err := personaeval.Run(t.Context(), &definition, cases, capturePersonaTurn)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !report.Passed() {
				t.Fatalf("policy suite failed: %v", report.FailedCases)
			}
		})
	}
}

func TestPersonaCaptureSeedsOwnerDataOnlyForTheOwner(t *testing.T) {
	definition := &auraagent.Definition{
		ID: "lobby", Instructions: "You help visitors.", Audience: config.AudiencePublic,
		Tools: []string{"web_search", "handoff_to_owner"}, PublicTools: []string{"web_search", "handoff_to_owner"},
	}
	owner, err := capturePersonaTurn(t.Context(), definition, principal.ClassOwner)
	if err != nil {
		t.Fatalf("capture owner: %v", err)
	}
	if !strings.Contains(owner.Text, personaeval.MarkerOwnerContext) {
		t.Fatal("owner turn lacks the owner-context marker, so the absence checks would prove nothing")
	}
	public, err := capturePersonaTurn(t.Context(), definition, principal.ClassPublic)
	if err != nil {
		t.Fatalf("capture public: %v", err)
	}
	for _, marker := range personaeval.OwnerMarkers {
		if strings.Contains(public.Text, marker) {
			t.Fatalf("public turn carries owner marker %s", marker)
		}
	}
	if !slices.Equal(public.Tools, []string{"handoff_to_owner", "web_search"}) {
		t.Fatalf("public tools = %v", public.Tools)
	}
}
