package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	appconfig "github.com/anggasct/aura/internal/app/config"
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/modules/knowledge"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/store"
)

const knowledgeOwnerActionKind = "kb.owner_action.v1"

func newKnowledgeService(cfg *appconfig.Config, db *sql.DB, resolver egress.Resolver) (*knowledge.Service, error) {
	if cfg == nil {
		return nil, errors.New("knowledge config must not be nil")
	}
	if db == nil {
		return nil, errors.New("knowledge database must not be nil")
	}
	kbStore, err := knowledge.NewSQLiteStore(db)
	if err != nil {
		return nil, err
	}
	wiring, err := knowledge.NewEmbeddingWiring(cfg.Knowledge, resolver)
	if err != nil {
		return nil, err
	}
	return knowledge.NewService(&knowledge.Deps{
		Store:     kbStore,
		Secrets:   &knowledgeSecrets{cfg: cfg},
		Reducer:   toolsbuiltin.ReduceHTML,
		HTTP:      knowledge.NewHTTPClient(resolver),
		Actions:   &knowledgeActions{db: db},
		Vectors:   kbStore,
		Embedder:  wiring.Embedder,
		Embedding: wiring.Settings,
	})
}

type knowledgeSecrets struct {
	cfg *appconfig.Config
}

func (s *knowledgeSecrets) Snapshot(_ context.Context) (knowledge.SecretScreen, error) {
	vault, err := buildStrictSecretVault(s.cfg)
	if err != nil {
		return nil, err
	}
	return vault, nil
}

type knowledgeActions struct {
	db *sql.DB
}

func (a *knowledgeActions) RecordOwnerAction(ctx context.Context, action *knowledge.OwnerAction) error {
	if action == nil {
		return errors.New("cli: knowledge owner action must not be nil")
	}
	sessionID, err := ensureOwnerActionSession(ctx, a.db, principal.OwnerID)
	if err != nil {
		return err
	}
	at := action.At.UTC().Format(time.RFC3339Nano)
	payload, err := json.Marshal(struct {
		Action     string `json:"action"`
		Collection string `json:"collection"`
		SourceID   string `json:"source_id"`
		Kind       string `json:"kind,omitempty"`
		Outcome    string `json:"outcome"`
		Chunks     int    `json:"chunks"`
		At         string `json:"at"`
	}{
		Action: action.Action, Collection: action.Collection, SourceID: action.SourceID,
		Kind: action.Kind, Outcome: action.Outcome, Chunks: action.Chunks, At: at,
	})
	if err != nil {
		return fmt.Errorf("cli: marshal knowledge owner action: %w", err)
	}
	_, _, err = turnstore.NewEventStore(a.db).UpsertEvent(ctx, &turn.Event{
		ID:            "kb-owner-action-" + action.Action + "-" + action.SourceID + "-" + at,
		SessionID:     sessionID,
		TurnID:        "owner-action",
		InvocationID:  "owner-action",
		Author:        principal.OwnerID,
		Kind:          knowledgeOwnerActionKind,
		SchemaVersion: 1,
		Payload:       payload,
		CreatedAt:     action.At.UTC(),
	})
	return err
}

func personaKnowledgeScopes(agents *auraagent.Config) []knowledge.PersonaScope {
	var scopes []knowledge.PersonaScope
	if agents == nil {
		return nil
	}
	for i := range agents.Definitions {
		definition := &agents.Definitions[i]
		for _, list := range []struct {
			class       string
			collections []string
		}{
			{"owner", definition.Knowledge},
			{"trusted", definition.PrincipalKnowledge},
			{"public", definition.PublicKnowledge},
		} {
			if len(list.collections) > 0 {
				scopes = append(scopes, knowledge.PersonaScope{
					Persona:     definition.ID,
					Class:       list.class,
					Collections: list.collections,
				})
			}
		}
	}
	return scopes
}

func knowledgeStatusChecks(cfg *appconfig.Config) ([]health.RegisteredCheck, error) {
	if cfg == nil {
		return nil, nil
	}
	dbPath, _, _, err := storagePaths(cfg)
	if err != nil {
		return nil, err
	}
	return knowledge.HealthChecks(cfg.Knowledge, personaKnowledgeScopes(cfg.Agents), dbPath, time.Duration(cfg.Health.CheckTimeout)), nil
}
