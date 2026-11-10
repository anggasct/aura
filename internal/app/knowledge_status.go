package app

import (
	"context"
	"os"
	"slices"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/store"
)

type personaKnowledgeScope struct {
	persona     string
	class       string
	collections []string
}

func configuredKnowledgeScopes(cfg *config.Config) []personaKnowledgeScope {
	var scopes []personaKnowledgeScope
	if cfg == nil || cfg.Agents == nil {
		return nil
	}
	for i := range cfg.Agents.Definitions {
		definition := &cfg.Agents.Definitions[i]
		for _, list := range []struct {
			class       string
			collections []string
		}{
			{"owner", definition.Knowledge},
			{"trusted", definition.PrincipalKnowledge},
			{"public", definition.PublicKnowledge},
		} {
			if len(list.collections) > 0 {
				scopes = append(scopes, personaKnowledgeScope{persona: definition.ID, class: list.class, collections: list.collections})
			}
		}
	}
	return scopes
}

func knowledgeScopeReader(cfg *config.Config, dbPath string) func(ctx context.Context) ([]health.KnowledgeScope, bool, error) {
	configured := configuredKnowledgeScopes(cfg)
	return func(ctx context.Context) ([]health.KnowledgeScope, bool, error) {
		if _, err := os.Stat(dbPath); err != nil {
			return nil, false, err
		}
		db, err := store.OpenReadOnly(ctx, dbPath)
		if err != nil {
			return nil, false, err
		}
		defer func() { _ = db.Close() }()
		present, err := store.KBIndexPresent(ctx, db)
		if err != nil || !present {
			return nil, present, err
		}
		collections, err := store.NewKBStore(db).ListCollections(ctx)
		if err != nil {
			return nil, true, err
		}
		sizes := make(map[string]int, len(collections))
		for i := range collections {
			sizes[collections[i].ID] = collections[i].Chunks
		}
		scopes := make([]health.KnowledgeScope, 0, len(configured))
		for _, scope := range configured {
			status := health.KnowledgeScope{Persona: scope.persona, Class: scope.class}
			for _, id := range scope.collections {
				size, known := sizes[id]
				if !known {
					status.Unknown = append(status.Unknown, id)
					continue
				}
				status.Chunks += size
			}
			slices.Sort(status.Unknown)
			scopes = append(scopes, status)
		}
		return scopes, true, nil
	}
}
