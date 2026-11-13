package config

import (
	"errors"
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

func TestLoad_PersonaKnowledgeRejectsSinksInTheMatchingClassList(t *testing.T) {
	tests := map[string]struct {
		fields string
		detail string
	}{
		"web_fetch for the public":      {fields: "      knowledge: [faq]\n      public_knowledge: [faq]\n      public_tools: [web_fetch]\n", detail: "public_tools lists \"web_fetch\""},
		"web_search for the public":     {fields: "      knowledge: [faq]\n      public_knowledge: [faq]\n      public_tools: [kb_search, web_search]\n", detail: "public_tools lists \"web_search\""},
		"workflow_start for the public": {fields: "      knowledge: [faq]\n      public_knowledge: [faq]\n      public_tools: [workflow_start]\n", detail: "public_tools lists \"workflow_start\""},
		"workflow_propose for trusted":  {fields: "      knowledge: [faq]\n      principal_knowledge: [faq]\n      principal_tools: [workflow_propose]\n", detail: "principal_tools lists \"workflow_propose\""},
		"web_fetch for trusted":         {fields: "      knowledge: [faq]\n      principal_knowledge: [faq]\n      principal_tools: [read_file, web_fetch]\n", detail: "principal_tools lists \"web_fetch\""},
		"mcp server for trusted":        {fields: "      knowledge: [faq]\n      principal_knowledge: [faq]\n      principal_tools: [mcp_docs]\n", detail: "principal_tools lists \"mcp_docs\""},
		"plugin for trusted":            {fields: "      knowledge: [faq]\n      principal_knowledge: [faq]\n      principal_tools: [plugin_weather]\n", detail: "principal_tools lists \"plugin_weather\""},
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

func TestLoad_PersonaKnowledgeTreatsUnknownToolsAsSinks(t *testing.T) {
	const prefix = "      knowledge: [faq]\n      principal_knowledge: [faq]\n"
	tests := map[string]struct {
		tools  string
		detail string
	}{
		"github_comment": {tools: "[github_comment]", detail: "which is not a known tool or sends data out"},
		"github_merge":   {tools: "[github_merge]", detail: "which is not a known tool or sends data out"},
		"web_fetch":      {tools: "[web_fetch]", detail: "which sends data out"},
		"read_file":      {tools: "[read_file]"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeDiscordConfig(t, personaKnowledgeConfig(prefix+"      principal_tools: "+test.tools+"\n")))
			if test.detail == "" {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				return
			}
			var coded *Error
			if !errors.As(err, &coded) || coded.Code != ErrorCodePersonaInvalid || !strings.Contains(coded.Detail, test.detail) {
				t.Fatalf("error = %v, want persona_invalid with %q", err, test.detail)
			}
			if test.detail == "which sends data out" && strings.Contains(coded.Detail, "not a known tool") {
				t.Fatalf("detail = %q, want the known-sink wording", coded.Detail)
			}
		})
	}
}

func TestLoad_PersonaKnowledgeAllowsSinksWhereTheClassHasNoKnowledge(t *testing.T) {
	for name, fields := range map[string]string{
		"owner knowledge only, sinks for trusted":     "      knowledge: [faq]\n      principal_tools: [web_fetch]\n",
		"trusted knowledge, sink for public only":     "      knowledge: [faq]\n      principal_knowledge: [faq]\n      principal_tools: [read_file, kb_search]\n      public_tools: [web_search]\n",
		"public knowledge, sink for trusted only":     "      knowledge: [faq]\n      public_knowledge: [faq]\n      public_tools: [kb_search]\n      principal_tools: [web_fetch]\n",
		"no knowledge at all":                         "      public_tools: [web_fetch]\n      principal_tools: [web_fetch]\n",
		"read tier tools beside class knowledge":      "      knowledge: [faq]\n      public_knowledge: [faq]\n      public_tools: [read_file, list_dir, kb_search]\n",
		"owner tools keep sinks with owner knowledge": "      knowledge: [faq]\n      tools: [web_fetch, kb_search]\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeDiscordConfig(t, personaKnowledgeConfig(fields))); err != nil {
				t.Fatalf("Load: %v", err)
			}
		})
	}
}
