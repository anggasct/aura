package config

import (
	"fmt"
	"regexp"
	"slices"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/tools"
)

const (
	KnowledgeCollectionIDPattern = `^[a-z][a-z0-9-]{1,47}$`
	MaxPersonaCollections        = 32
)

var knowledgeCollectionID = regexp.MustCompile(KnowledgeCollectionIDPattern)

func ValidKnowledgeCollectionID(id string) bool {
	return knowledgeCollectionID.MatchString(id)
}

func ValidatePersonaKnowledge(field string, definition *AgentDefinition) error {
	lists := []struct {
		name        string
		collections []string
	}{
		{"knowledge", definition.Knowledge},
		{"principal_knowledge", definition.PrincipalKnowledge},
		{"public_knowledge", definition.PublicKnowledge},
	}
	for _, list := range lists {
		if len(list.collections) > MaxPersonaCollections {
			return configkit.Field(field+"."+list.name, fault.New(ErrorCodePersonaInvalid, fmt.Sprintf("must list at most %d collections", MaxPersonaCollections)))
		}
		seen := make(map[string]struct{}, len(list.collections))
		for _, id := range list.collections {
			if !ValidKnowledgeCollectionID(id) {
				return configkit.Field(field+"."+list.name, fault.New(ErrorCodePersonaInvalid, "entries must match "+KnowledgeCollectionIDPattern))
			}
			if _, duplicate := seen[id]; duplicate {
				return configkit.Field(field+"."+list.name, fault.New(ErrorCodePersonaInvalid, "must not list a collection twice"))
			}
			seen[id] = struct{}{}
		}
	}
	for _, list := range lists[1:] {
		for _, id := range list.collections {
			if !slices.Contains(definition.Knowledge, id) {
				return configkit.Field(field+"."+list.name, fault.New(ErrorCodePersonaInvalid, fmt.Sprintf("lists %q, which is not in %s.knowledge", id, field)))
			}
		}
	}
	return rejectKnowledgeSinks(field, definition)
}

func rejectKnowledgeSinks(field string, definition *AgentDefinition) error {
	classes := []struct {
		toolsField     string
		toolList       []string
		knowledgeField string
		knowledge      []string
	}{
		{"principal_tools", definition.PrincipalTools, "principal_knowledge", definition.PrincipalKnowledge},
		{"public_tools", definition.PublicTools, "public_knowledge", definition.PublicKnowledge},
	}
	for _, class := range classes {
		if len(class.knowledge) == 0 {
			continue
		}
		for _, entry := range class.toolList {
			sink, known := isExternalSinkEntry(entry)
			if !sink {
				continue
			}
			reason := "which sends data out"
			if !known {
				reason = "which is not a known tool or sends data out"
			}
			return configkit.Field(field+"."+class.toolsField, fault.New(ErrorCodePersonaInvalid, fmt.Sprintf("lists %q, %s, while %s.%s is not empty", entry, reason, field, class.knowledgeField)))
		}
	}
	return nil
}

func isExternalSinkEntry(entry string) (sink, known bool) {
	if tools.IsMCPEntry(entry) || tools.IsPluginEntry(entry) {
		return true, true
	}
	builtins := tools.Builtins()
	for i := range builtins {
		if builtins[i].Name == entry {
			return builtins[i].IsExternalSink(), true
		}
	}
	return true, false
}
