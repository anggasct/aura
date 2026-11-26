package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	appconfig "github.com/anggasct/aura/internal/app/config"
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/modules/knowledge"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite"
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
	wiring, err := newKnowledgeEmbedding(cfg, resolver)
	if err != nil {
		return nil, err
	}
	return knowledge.NewService(&knowledge.Deps{
		Store:     kbStore,
		Secrets:   &knowledgeSecrets{cfg: cfg},
		Reducer:   toolsbuiltin.ReduceHTML,
		HTTP:      newKnowledgeHTTPClient(resolver),
		Actions:   &knowledgeActions{db: db},
		Vectors:   kbStore,
		Embedder:  wiring.embedder,
		Embedding: wiring.settings,
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

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func refuseDeniedEgress(err error) error {
	if fault.HasCode(err, egress.ErrorCodeEgressDenied) {
		return knowledge.Errorf(knowledge.ErrorCodeURLRefused, "destination is not permitted")
	}
	return err
}

func newKnowledgeHTTPClient(resolver egress.Resolver) *http.Client {
	client := egress.NewAPIClient(resolver)
	transport := client.Transport
	client.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		response, err := transport.RoundTrip(request)
		return response, refuseDeniedEgress(err)
	})
	redirect := client.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		return refuseDeniedEgress(redirect(request, via))
	}
	return client
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

const knowledgeReadPoolConns = 4

func openKnowledgeReadPool(ctx context.Context, dbPath string) (*sql.DB, error) {
	pool, err := sqlite.OpenReadOnly(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(knowledgeReadPoolConns)
	return pool, nil
}

func newKnowledgeRetriever(cfg *appconfig.Config, pool *sql.DB, opts ...knowledge.RetrieverOption) (*knowledge.Retriever, error) {
	if cfg == nil {
		return nil, errors.New("knowledge config must not be nil")
	}
	if pool == nil {
		return nil, errors.New("knowledge read pool must not be nil")
	}
	index, err := knowledge.NewSQLiteIndex(pool)
	if err != nil {
		return nil, err
	}
	return knowledge.NewRetriever(index, &knowledgeSecrets{cfg: cfg}, opts...)
}

type knowledgeRuntime struct {
	retriever *knowledge.Retriever
	provider  *knowledge.ContextProvider
	pool      *sql.DB
	vectors   *knowledgeVectorRuntime
}

type knowledgeToolSearcher struct {
	retriever *knowledge.Retriever
}

func (s *knowledgeToolSearcher) SearchKnowledge(ctx context.Context, scope []string, query string, limit int, class principal.Class) (json.RawMessage, error) {
	budgeted, cancel := context.WithTimeout(ctx, knowledge.RetrievalBudget)
	defer cancel()
	found, err := s.retriever.Search(budgeted, &knowledge.SearchRequest{Scope: scope, Query: query, Depth: knowledge.SearchDepth, Class: class})
	if err != nil {
		return nil, err
	}
	output := knowledge.ShapeSearch(found.Hits, limit)
	output.Mode = found.Mode
	return json.Marshal(output)
}

func (r *knowledgeRuntime) attachSearch(builtin *toolsbuiltin.Executor) {
	if r == nil || builtin == nil {
		return
	}
	builtin.SetKnowledgeSearcher(&knowledgeToolSearcher{retriever: r.retriever})
}

func (r *knowledgeRuntime) close() {
	if r == nil {
		return
	}
	r.vectors.close()
	if r.pool != nil {
		_ = r.pool.Close()
	}
}

func personasListKnowledge(registry *auraagent.Registry) bool {
	definitions := registry.Definitions()
	for i := range definitions {
		if len(definitions[i].Knowledge) > 0 {
			return true
		}
	}
	return false
}

func buildKnowledgeRuntime(ctx context.Context, cfg *appconfig.Config, registry *auraagent.Registry) (rt *knowledgeRuntime, enabled bool, err error) {
	if !personasListKnowledge(registry) {
		return nil, false, nil
	}
	dbPath, _, _, err := storagePaths(cfg)
	if err != nil {
		return nil, false, err
	}
	pool, err := openKnowledgeReadPool(ctx, dbPath)
	if err != nil {
		return nil, false, err
	}
	vectors, _, err := startKnowledgeVectors(ctx, cfg, pool, dbPath)
	if err != nil {
		_ = pool.Close()
		return nil, false, err
	}
	retriever, err := newKnowledgeRetriever(cfg, pool, vectors.options()...)
	if err != nil {
		vectors.close()
		_ = pool.Close()
		return nil, false, err
	}
	provider, err := knowledge.NewContextProvider(retriever)
	if err != nil {
		vectors.close()
		_ = pool.Close()
		return nil, false, err
	}
	return &knowledgeRuntime{retriever: retriever, provider: provider, pool: pool, vectors: vectors}, true, nil
}
