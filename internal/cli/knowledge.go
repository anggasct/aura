package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/egress"
	"github.com/anggasct/aura/internal/knowledge"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/store"
	toolsbuiltin "github.com/anggasct/aura/internal/tools/builtin"
)

const knowledgeOwnerActionKind = "kb.owner_action.v1"

func newKnowledgeService(cfg *config.Config, db *sql.DB, resolver egress.Resolver) (*knowledge.Service, error) {
	if cfg == nil {
		return nil, errors.New("knowledge config must not be nil")
	}
	if db == nil {
		return nil, errors.New("knowledge database must not be nil")
	}
	return knowledge.NewService(&knowledge.Deps{
		Store:   &knowledgeStore{store: store.NewKBStore(db)},
		Secrets: &knowledgeSecrets{cfg: cfg},
		Reducer: toolsbuiltin.ReduceHTML,
		HTTP:    newKnowledgeHTTPClient(resolver),
		Actions: &knowledgeActions{db: db},
	})
}

type knowledgeSecrets struct {
	cfg *config.Config
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
	if code, ok := egress.CodeOf(err); ok && code == egress.ErrorCodeEgressDenied {
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
	_, _, err = store.NewEventStore(a.db).UpsertEvent(ctx, &store.RuntimeEvent{
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

type knowledgeStore struct {
	store store.KBStore
}

func translateKnowledgeStoreError(err error) error {
	code, ok := store.CodeOf(err)
	if !ok {
		return err
	}
	switch code {
	case store.ErrorCodeKBCapExceeded:
		return knowledge.Errorf(knowledge.ErrorCodeCapExceeded, "chunk cap would be exceeded")
	case store.ErrorCodeKBSourceNotFound:
		return knowledge.Errorf(knowledge.ErrorCodeSourceNotFound, "source does not exist in the collection")
	default:
		return err
	}
}

func (s *knowledgeStore) ReplaceSource(ctx context.Context, write *knowledge.SourceWrite) (knowledge.WriteResult, error) {
	if write == nil {
		return knowledge.WriteResult{}, knowledge.Errorf(knowledge.ErrorCodeInvalidArgument, "write must not be nil")
	}
	chunks := make([]store.KBChunk, 0, len(write.Chunks))
	for i := range write.Chunks {
		chunks = append(chunks, store.KBChunk{
			Ordinal: write.Chunks[i].Ordinal, Heading: write.Chunks[i].Heading,
			Content: write.Chunks[i].Content, SHA256: write.Chunks[i].SHA256,
		})
	}
	result, err := s.store.ReplaceSource(ctx, &store.KBSourceWrite{
		Collection: write.Collection, Kind: write.Kind, URI: write.URI, Label: write.Label,
		ContentSHA256: write.ContentSHA256, IngestVersion: write.IngestVersion, IngestedAt: write.IngestedAt,
		Chunks: chunks, Limits: store.KBLimits{CollectionChunks: write.CollectionCap, TotalChunks: write.TotalCap},
	})
	if err != nil {
		return knowledge.WriteResult{}, translateKnowledgeStoreError(err)
	}
	return knowledge.WriteResult{
		Status: result.Status, SourceID: result.SourceID, Chunks: result.Chunks,
		ChunksAdded: result.ChunksAdded, ChunksRemoved: result.ChunksRemoved, ChunksKept: result.ChunksKept,
	}, nil
}

func (s *knowledgeStore) ListCollections(ctx context.Context) ([]knowledge.CollectionSummary, error) {
	collections, err := s.store.ListCollections(ctx)
	if err != nil {
		return nil, translateKnowledgeStoreError(err)
	}
	out := make([]knowledge.CollectionSummary, 0, len(collections))
	for i := range collections {
		out = append(out, knowledge.CollectionSummary{
			ID: collections[i].ID, Generation: collections[i].Generation, Sources: collections[i].Sources,
			Chunks: collections[i].Chunks, CreatedAt: collections[i].CreatedAt,
		})
	}
	return out, nil
}

func (s *knowledgeStore) ListSources(ctx context.Context, collection string) ([]knowledge.SourceRecord, error) {
	sources, err := s.store.ListSources(ctx, collection)
	if err != nil {
		return nil, translateKnowledgeStoreError(err)
	}
	out := make([]knowledge.SourceRecord, 0, len(sources))
	for i := range sources {
		out = append(out, knowledge.SourceRecord{
			ID: sources[i].ID, Collection: sources[i].Collection, Kind: sources[i].Kind, URI: sources[i].URI,
			Label: sources[i].Label, ContentSHA256: sources[i].ContentSHA256, IngestVersion: sources[i].IngestVersion,
			IngestedAt: sources[i].IngestedAt, Chunks: sources[i].Chunks,
		})
	}
	return out, nil
}

func (s *knowledgeStore) RemoveSource(ctx context.Context, collection, uri string, apply bool) (knowledge.RemoveOutcome, error) {
	result, err := s.store.RemoveSource(ctx, collection, uri, apply)
	outcome := knowledge.RemoveOutcome{SourceID: result.SourceID, Chunks: result.Chunks, Applied: result.Applied, WALTruncated: result.WALTruncated}
	return outcome, translateKnowledgeStoreError(err)
}

type knowledgeIndex struct {
	index store.KBIndex
}

func (i *knowledgeIndex) ScopeChunks(ctx context.Context, collections []string) (int, error) {
	return i.index.ScopeChunks(ctx, collections)
}

func (i *knowledgeIndex) TermFrequency(ctx context.Context, collections []string, term string, limit int) (int, error) {
	return i.index.TermFrequency(ctx, collections, term, limit)
}

func (i *knowledgeIndex) Search(ctx context.Context, collections, terms []string, depth int) ([]knowledge.Hit, error) {
	found, err := i.index.Search(ctx, collections, terms, depth)
	if err != nil {
		return nil, err
	}
	hits := make([]knowledge.Hit, 0, len(found))
	for n := range found {
		hits = append(hits, knowledge.Hit{
			ChunkID: found[n].ChunkID, SourceID: found[n].SourceID, Collection: found[n].Collection,
			Label: found[n].Label, Heading: found[n].Heading, Ordinal: found[n].Ordinal,
			Content: found[n].Content, Score: found[n].Score,
		})
	}
	return hits, nil
}

const knowledgeReadPoolConns = 4

func openKnowledgeReadPool(ctx context.Context, dbPath string) (*sql.DB, error) {
	pool, err := store.OpenReadOnly(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(knowledgeReadPoolConns)
	return pool, nil
}

func newKnowledgeRetriever(cfg *config.Config, pool *sql.DB) (*knowledge.Retriever, error) {
	if cfg == nil {
		return nil, errors.New("knowledge config must not be nil")
	}
	if pool == nil {
		return nil, errors.New("knowledge read pool must not be nil")
	}
	return knowledge.NewRetriever(&knowledgeIndex{index: store.NewKBIndex(pool)}, &knowledgeSecrets{cfg: cfg})
}

func (s *knowledgeStore) Check(ctx context.Context) (knowledge.IndexReport, error) {
	report, err := s.store.Check(ctx)
	if err != nil {
		return knowledge.IndexReport{}, translateKnowledgeStoreError(err)
	}
	return knowledge.IndexReport{
		Collections: report.Collections, Sources: report.Sources, Chunks: report.Chunks,
		IndexedChunks: report.IndexedChunks, Problems: report.Problems,
	}, nil
}
