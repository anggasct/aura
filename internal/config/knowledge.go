package config

import (
	"fmt"
	"regexp"
	"slices"

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

func validatePersonaKnowledge(field string, definition *AgentDefinition) error {
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
			return &Error{Code: ErrorCodePersonaInvalid, Detail: fmt.Sprintf("%s.%s must list at most %d collections", field, list.name, MaxPersonaCollections)}
		}
		seen := make(map[string]struct{}, len(list.collections))
		for _, id := range list.collections {
			if !ValidKnowledgeCollectionID(id) {
				return &Error{Code: ErrorCodePersonaInvalid, Detail: fmt.Sprintf("%s.%s entries must match %s", field, list.name, KnowledgeCollectionIDPattern)}
			}
			if _, duplicate := seen[id]; duplicate {
				return &Error{Code: ErrorCodePersonaInvalid, Detail: fmt.Sprintf("%s.%s must not list a collection twice", field, list.name)}
			}
			seen[id] = struct{}{}
		}
	}
	for _, list := range lists[1:] {
		for _, id := range list.collections {
			if !slices.Contains(definition.Knowledge, id) {
				return &Error{Code: ErrorCodePersonaInvalid, Detail: fmt.Sprintf("%s.%s lists %q, which is not in %s.knowledge", field, list.name, id, field)}
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
			if isExternalSinkEntry(entry) {
				return &Error{Code: ErrorCodePersonaInvalid, Detail: fmt.Sprintf("%s.%s lists %q, which sends data out, while %s.%s is not empty", field, class.toolsField, entry, field, class.knowledgeField)}
			}
		}
	}
	return nil
}

func isExternalSinkEntry(entry string) bool {
	if tools.IsMCPEntry(entry) || tools.IsPluginEntry(entry) {
		return true
	}
	builtins := tools.Builtins()
	for i := range builtins {
		if builtins[i].Name == entry {
			return builtins[i].IsExternalSink()
		}
	}
	return false
}
