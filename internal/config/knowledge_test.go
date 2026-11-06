package config

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func personaKnowledgeConfig(fields string) string {
	return "version: 1\nagents:\n  definitions:\n    - id: lobby\n      description: Front desk\n      instructions: Help visitors.\n      audience: public\n" + fields
}

func TestLoad_PersonaKnowledgeLists(t *testing.T) {
	path := writeDiscordConfig(t, personaKnowledgeConfig(`      knowledge: [support, hr-docs, faq]
      principal_knowledge: [support, faq]
      public_knowledge: [faq]
`))
	result, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	definition := result.Config.Agents.Definitions[0]
	if !slices.Equal(definition.Knowledge, []string{"support", "hr-docs", "faq"}) ||
		!slices.Equal(definition.PrincipalKnowledge, []string{"support", "faq"}) ||
		!slices.Equal(definition.PublicKnowledge, []string{"faq"}) {
		t.Fatalf("lists = %v %v %v", definition.Knowledge, definition.PrincipalKnowledge, definition.PublicKnowledge)
	}
}

func TestLoad_PersonaKnowledgeRejectsInvalidLists(t *testing.T) {
	tooMany := make([]string, 0, MaxPersonaCollections+1)
	for i := range MaxPersonaCollections + 1 {
		tooMany = append(tooMany, fmt.Sprintf("col-%02d", i))
	}
	tests := map[string]struct {
		fields string
		detail string
	}{
		"uppercase id":          {fields: "      knowledge: [Support]\n", detail: "knowledge entries must match"},
		"short id":              {fields: "      knowledge: [a]\n", detail: "knowledge entries must match"},
		"leading digit":         {fields: "      knowledge: [1abc]\n", detail: "knowledge entries must match"},
		"space":                 {fields: "      knowledge: [\"a b\"]\n", detail: "knowledge entries must match"},
		"duplicate":             {fields: "      knowledge: [support, support]\n", detail: "knowledge must not list a collection twice"},
		"too many":              {fields: "      knowledge: [" + strings.Join(tooMany, ", ") + "]\n", detail: "at most 32"},
		"public outside":        {fields: "      knowledge: [support]\n      public_knowledge: [faq]\n", detail: "public_knowledge lists \"faq\""},
		"principal outside":     {fields: "      principal_knowledge: [faq]\n", detail: "principal_knowledge lists \"faq\""},
		"public duplicate":      {fields: "      knowledge: [faq]\n      public_knowledge: [faq, faq]\n", detail: "public_knowledge must not list"},
		"not a list":            {fields: "      knowledge: support\n", detail: "must be a list"},
		"non string entry":      {fields: "      knowledge: [1]\n", detail: "entries must be strings"},
		"principal bad pattern": {fields: "      knowledge: [faq]\n      principal_knowledge: [FAQ]\n", detail: "principal_knowledge entries must match"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeDiscordConfig(t, personaKnowledgeConfig(test.fields)))
			if err == nil || !strings.Contains(err.Error(), test.detail) {
				t.Fatalf("error = %v, want detail %q", err, test.detail)
			}
		})
	}
}

func TestValidKnowledgeCollectionID(t *testing.T) {
	for id, want := range map[string]bool{"ab": true, "hr-docs": true, "a1-b2": true, "a": false, "Ab": false, "-ab": false, "ab_": false, strings.Repeat("a", 48): true, strings.Repeat("a", 49): false, "": false} {
		if got := ValidKnowledgeCollectionID(id); got != want {
			t.Errorf("ValidKnowledgeCollectionID(%q) = %t, want %t", id, got, want)
		}
	}
}
