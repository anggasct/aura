package app

import (
	"slices"
	"testing"

	"github.com/anggasct/aura/internal/platform/principal"
)

func TestRegistryPersonasListKnowledgeByClassFromTheConfig(t *testing.T) {
	path := writeAgentsConfig(t, `storage:
  path: `+t.TempDir()+`
agents:
  definitions:
    - id: lobby
      description: Front desk
      instructions: Help visitors.
      audience: public
      knowledge: [support, hr-docs, faq]
      principal_knowledge: [support, faq]
      public_knowledge: [faq]
    - id: plain
      description: No knowledge
      instructions: Chat.
`)
	seedConfiguredStorage(t, path)
	personas := registryPersonas{registry: personaRegistry(t, path)}

	tests := map[string]struct {
		agent string
		class principal.Class
		want  []string
	}{
		"owner":             {agent: "lobby", class: principal.ClassOwner, want: []string{"support", "hr-docs", "faq"}},
		"trusted":           {agent: "lobby", class: principal.ClassTrusted, want: []string{"support", "faq"}},
		"public":            {agent: "lobby", class: principal.ClassPublic, want: []string{"faq"}},
		"unclassified":      {agent: "lobby", class: "", want: nil},
		"unknown class":     {agent: "lobby", class: "admin", want: nil},
		"no knowledge":      {agent: "plain", class: principal.ClassOwner, want: nil},
		"unregistered":      {agent: "ghost", class: principal.ClassOwner, want: nil},
		"builtin default":   {agent: "main", class: principal.ClassOwner, want: nil},
		"public of persona": {agent: "plain", class: principal.ClassPublic, want: nil},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got := personas.Knowledge(test.agent, test.class); !slices.Equal(got, test.want) {
				t.Errorf("Knowledge(%q, %q) = %v, want %v", test.agent, test.class, got, test.want)
			}
		})
	}
}
