package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/anggasct/aura/internal/platform/embedding"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/secret"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
)

type embedCall struct {
	role  embedding.Role
	texts []string
}

type deterministicEmbedder struct {
	mu      sync.Mutex
	vectors map[string][]float32
	calls   []embedCall
	err     error
	delay   time.Duration
	onEmbed func(role embedding.Role, texts []string)
}

func (e *deterministicEmbedder) Embed(ctx context.Context, role embedding.Role, texts []string) ([][]float32, error) {
	e.mu.Lock()
	e.calls = append(e.calls, embedCall{role: role, texts: slices.Clone(texts)})
	err := e.err
	delay := e.delay
	vecs := e.vectors
	onEmbed := e.onEmbed
	e.mu.Unlock()
	if onEmbed != nil {
		onEmbed(role, texts)
	}
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if err != nil {
		return nil, err
	}
	out := make([][]float32, len(texts))
	for i, text := range texts {
		if vec, ok := vecs[text]; ok {
			out[i] = slices.Clone(vec)
			continue
		}
		out[i] = []float32{0, 0, 0, 1}
	}
	return out, nil
}

func (e *deterministicEmbedder) callList() []embedCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.calls)
}

func (e *deterministicEmbedder) queryCallCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, call := range e.calls {
		if call.role == embedding.RoleQuery {
			n++
		}
	}
	return n
}

func (e *deterministicEmbedder) sentContains(needle string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, call := range e.calls {
		for _, text := range call.texts {
			if strings.Contains(text, needle) {
				return true
			}
		}
	}
	return false
}

func (e *deterministicEmbedder) resetCalls() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = nil
}

type staticSecretSource struct {
	screen secret.Screen
	err    error
}

func (s staticSecretSource) Snapshot(context.Context) (secret.Screen, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.screen, nil
}

func seedMemorySessionWithAgent(t *testing.T, db *sql.DB, id, ownerID, agentID string) {
	t.Helper()
	now := sqlite.FormatTime(time.Now().UTC())
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO session (id, owner_id, agent_id, metadata_json, created_at, updated_at) VALUES (?, ?, ?, '{}', ?, ?)`,
		id, ownerID, agentID, now, now,
	); err != nil {
		t.Fatalf("seed session %s: %v", id, err)
	}
}

func seedHybridDoc(t *testing.T, store *SQLiteStore, id, ownerID, sessionID, content string, seq int64, createdAt time.Time, tainted bool) {
	t.Helper()
	doc := &StoredDocument{
		ID:           id,
		OwnerID:      ownerID,
		SessionID:    sessionID,
		Kind:         KindEventText,
		FromSequence: seq,
		ToSequence:   seq,
		Content:      content,
		TrustLabel:   "owner_input",
		Tainted:      &tainted,
		CreatedAt:    createdAt,
	}
	if _, err := store.UpsertDocument(t.Context(), doc); err != nil {
		t.Fatalf("UpsertDocument(%s): %v", id, err)
	}
}

type signalingVectorStore struct {
	*SQLiteStore
	upserted chan struct{}
}

func (s *signalingVectorStore) UpsertEmbeddings(ctx context.Context, fingerprint string, writes []EmbeddingWrite) error {
	err := s.SQLiteStore.UpsertEmbeddings(ctx, fingerprint, writes)
	if err == nil {
		select {
		case s.upserted <- struct{}{}:
		default:
		}
	}
	return err
}

func countMemoryEmbeddings(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM memory_embedding`).Scan(&n); err != nil {
		t.Fatalf("count memory_embedding: %v", err)
	}
	return n
}

func TestMemoryHybridSearchMatchesSynonymsAndFusesWithRRF(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

	t.Run("synonyms across primary and fallback use single query embed and emit hybrid telemetry", func(t *testing.T) {
		db, store := newTestSQLiteStore(t)
		seedMemorySessionWithAgent(t, db, "sess-primary", principal.OwnerID, "main")
		seedMemorySessionWithAgent(t, db, "sess-other", principal.OwnerID, "main")

		embedder := &deterministicEmbedder{
			vectors: map[string][]float32{
				"pesawat ke Tokyo":                  {1, 0, 0, 0},
				"terbang ke Jepang minggu depan":    {0.98, 0.2, 0, 0},
				"jadwal penerbangan liburan Tokyo":  {0.96, 0.28, 0, 0},
				"catatan belanja sayur di pasar":    {0, 1, 0, 0},
				"dokumen tercemar terbang ke Tokyo": {0.95, 0.31, 0, 0},
			},
		}
		service, err := NewService(store, nil, &ServiceOptions{
			EmbeddingEnabled: true,
			Embedder:         embedder,
			Embedding: &EmbeddingSettings{
				Fingerprint: "fp-v1",
				Dimensions:  4,
				BatchSize:   16,
			},
			Vectors: store,
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}

		seedHybridDoc(t, store, "mem-syn-other", principal.OwnerID, "sess-other", "terbang ke Jepang minggu depan", 1, now.Add(-2*time.Hour), false)
		seedHybridDoc(t, store, "mem-unrelated", principal.OwnerID, "sess-primary", "catatan belanja sayur di pasar", 1, now.Add(-time.Hour), false)
		if embedded, err := service.BackfillEmbeddingsOnce(t.Context(), 50); err != nil || embedded != 2 {
			t.Fatalf("BackfillEmbeddingsOnce = (%d, %v), want (2, nil)", embedded, err)
		}
		embedder.resetCalls()

		exporter := tracetest.NewInMemoryExporter()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
		var observed *Observation
		service.observer = func(_ context.Context, obs *Observation) { observed = obs }
		provider, err := NewProvider(service, &ProviderConfig{
			Scopes:         staticScopes{"main": {"main"}},
			TracerProvider: tp,
			Now:            func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("NewProvider: %v", err)
		}

		blocks, err := provider.TurnContext(t.Context(), &turn.TurnRequest{
			PrincipalID:    principal.OwnerID,
			PrincipalClass: principal.ClassOwner,
			AgentID:        "main",
			SessionID:      "sess-primary",
			Parts:          []runtimeingress.InputPart{{Text: "pesawat ke Tokyo"}},
		})
		if err != nil {
			t.Fatalf("TurnContext: %v", err)
		}
		if len(blocks) != 1 || !strings.Contains(blocks[0].Content, "terbang ke Jepang minggu depan") {
			t.Fatalf("TurnContext blocks = %+v, want semantic synonym match from fallback session", blocks)
		}
		if blocks[0].Taint != nil {
			t.Fatalf("clean semantic hit carried taint: %+v", blocks[0].Taint)
		}
		if got := embedder.queryCallCount(); got != 1 {
			t.Fatalf("Embed(RoleQuery) calls = %d, want 1 across primary + fallback", got)
		}
		if observed == nil || observed.Mode != ModeHybrid {
			t.Fatalf("observed = %+v, want mode=%s", observed, ModeHybrid)
		}
		spans := exporter.GetSpans()
		if len(spans) != 1 {
			t.Fatalf("spans = %d, want 1", len(spans))
		}
		gotMode := ""
		for _, attr := range spans[0].Attributes {
			if string(attr.Key) == AttrRecallMode {
				gotMode = attr.Value.AsString()
			}
		}
		if gotMode != ModeHybrid {
			t.Fatalf("span %s = %q, want %q", AttrRecallMode, gotMode, ModeHybrid)
		}

		ops, err := NewOperations(&OperationDeps{
			Service: service,
			Scopes:  staticScopes{"main": {"main"}},
			Now:     func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("NewOperations: %v", err)
		}
		var searchHandle operation.Handler
		for _, b := range ops.Bindings() {
			if b.ID == operationSearch {
				searchHandle = b.Handle
			}
		}
		res, err := searchHandle(t.Context(), &operation.Call{
			Caller: operation.Caller{Surface: operation.SurfaceCLI, PrincipalID: principal.OwnerID, Class: principal.ClassOwner},
			Input:  json.RawMessage(`{"query":"pesawat ke Tokyo","limit":5}`),
		})
		if err != nil {
			t.Fatalf("memory.search: %v", err)
		}
		var payload struct {
			Mode string      `json:"mode"`
			Rows []searchRow `json:"rows"`
		}
		if err := json.Unmarshal(res.Data, &payload); err != nil {
			t.Fatalf("unmarshal search output: %v", err)
		}
		if payload.Mode != ModeHybrid || len(payload.Rows) != 1 || payload.Rows[0].ID != "mem-syn-other" {
			t.Fatalf("memory.search payload = %+v, want hybrid match mem-syn-other", payload)
		}

		seedHybridDoc(t, store, "mem-tainted-vec", principal.OwnerID, "sess-primary", "dokumen tercemar terbang ke Tokyo", 2, now.Add(-30*time.Minute), true)
		if _, err := service.BackfillEmbeddingsOnce(t.Context(), 50); err != nil {
			t.Fatalf("BackfillEmbeddingsOnce(tainted): %v", err)
		}
		taintedBlocks, err := provider.TurnContext(t.Context(), &turn.TurnRequest{
			PrincipalID:    principal.OwnerID,
			PrincipalClass: principal.ClassOwner,
			AgentID:        "main",
			SessionID:      "sess-primary",
			Parts:          []runtimeingress.InputPart{{Text: "pesawat ke Tokyo"}},
		})
		if err != nil {
			t.Fatalf("TurnContext(tainted): %v", err)
		}
		if len(taintedBlocks) != 1 || taintedBlocks[0].Taint == nil || taintedBlocks[0].Taint.Source != turn.TaintSourceRecall {
			t.Fatalf("tainted vector hit did not propagate recall taint: %+v", taintedBlocks)
		}
	})

	t.Run("RRF ranks dual lexical+semantic hits above single-leg hits", func(t *testing.T) {
		db, store := newTestSQLiteStore(t)
		seedMemorySessionWithAgent(t, db, "sess-rrf", principal.OwnerID, "main")

		embedder := &deterministicEmbedder{
			vectors: map[string][]float32{
				"deploy kubernetes":                   {1, 0, 0, 0},
				"deploy kubernetes cluster migration": {0.92, 0.39, 0, 0},
				"deploy kubernetes notes":             {0, 1, 0, 0},
				"rollout container orchestration":     {0.99, 0.14, 0, 0},
			},
		}
		service, err := NewService(store, nil, &ServiceOptions{
			EmbeddingEnabled: true,
			Embedder:         embedder,
			Embedding:        &EmbeddingSettings{Fingerprint: "fp-rrf", Dimensions: 4},
			Vectors:          store,
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		seedHybridDoc(t, store, "mem-dual", principal.OwnerID, "sess-rrf", "deploy kubernetes cluster migration", 1, now.Add(-2*time.Hour), false)
		seedHybridDoc(t, store, "mem-lex-only", principal.OwnerID, "sess-rrf", "deploy kubernetes notes", 2, now.Add(-time.Hour), false)
		seedHybridDoc(t, store, "mem-vec-only", principal.OwnerID, "sess-rrf", "rollout container orchestration", 3, now.Add(-time.Hour), false)
		if _, err := service.BackfillEmbeddingsOnce(t.Context(), 50); err != nil {
			t.Fatalf("BackfillEmbeddingsOnce: %v", err)
		}

		hits, mode, err := service.SearchWithMode(t.Context(), &SearchRequest{
			OwnerID:        principal.OwnerID,
			PrincipalClass: principal.ClassOwner,
			SessionID:      "sess-rrf",
			Query:          "deploy kubernetes",
			Now:            now,
			Limit:          5,
		})
		if err != nil {
			t.Fatalf("SearchWithMode: %v", err)
		}
		if mode != ModeHybrid {
			t.Fatalf("mode = %q, want %q", mode, ModeHybrid)
		}
		if len(hits) != 3 || hits[0].ID != "mem-dual" {
			t.Fatalf("hits = %+v, want mem-dual ranked #1 by RRF", hits)
		}
	})

	t.Run("persona scope, exact RFC3339Nano bounds, and bounded streaming scan cap enforced on vector leg", func(t *testing.T) {
		db, store := newTestSQLiteStore(t)
		seedMemorySessionWithAgent(t, db, "sess-alpha", principal.OwnerID, "alpha")
		seedMemorySessionWithAgent(t, db, "sess-beta", principal.OwnerID, "beta")
		seedMemorySessionWithAgent(t, db, "sess-shared-a", principal.OwnerID, "shared-a")
		seedMemorySessionWithAgent(t, db, "sess-shared-b", principal.OwnerID, "shared-b")

		embedder := &deterministicEmbedder{
			vectors: map[string][]float32{
				"secretive synonym query": {1, 0, 0, 0},
				"alpha private note":      {0.90, 0.43, 0, 0},
				"beta private note":       {0.99, 0.14, 0, 0},
				"shared note from b":      {0.97, 0.24, 0, 0},
				"boundary before doc":     {0.98, 0.19, 0, 0},
				"boundary inside doc":     {0.96, 0.28, 0, 0},
				"boundary after doc":      {0.99, 0.14, 0, 0},
			},
		}
		service, err := NewService(store, nil, &ServiceOptions{
			EmbeddingEnabled:  true,
			Embedder:          embedder,
			Embedding:         &EmbeddingSettings{Fingerprint: "fp-scope", Dimensions: 4},
			Vectors:           store,
			MaxVectorScanRows: 100,
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}

		seedHybridDoc(t, store, "mem-alpha", principal.OwnerID, "sess-alpha", "alpha private note", 1, now.Add(-time.Hour), false)
		seedHybridDoc(t, store, "mem-beta", principal.OwnerID, "sess-beta", "beta private note", 1, now.Add(-time.Hour), false)
		seedHybridDoc(t, store, "mem-shared-b", principal.OwnerID, "sess-shared-b", "shared note from b", 1, now.Add(-time.Hour), false)

		boundary := time.Date(2026, 11, 28, 10, 0, 0, 500_000_000, time.UTC)
		seedHybridDoc(t, store, "mem-sub-before", principal.OwnerID, "sess-alpha", "boundary before doc", 2, boundary.Add(-100*time.Millisecond), false)
		seedHybridDoc(t, store, "mem-sub-inside", principal.OwnerID, "sess-alpha", "boundary inside doc", 3, boundary.Add(100*time.Millisecond), false)
		seedHybridDoc(t, store, "mem-sub-after", principal.OwnerID, "sess-alpha", "boundary after doc", 4, boundary.Add(400*time.Millisecond), false)
		if _, err := service.BackfillEmbeddingsOnce(t.Context(), 50); err != nil {
			t.Fatalf("BackfillEmbeddingsOnce: %v", err)
		}

		alphaHits, err := service.Search(t.Context(), &SearchRequest{
			OwnerID:        principal.OwnerID,
			PrincipalClass: principal.ClassOwner,
			AgentIDs:       []string{"alpha"},
			Query:          "secretive synonym query",
			Now:            now,
			Limit:          10,
		})
		if err != nil {
			t.Fatalf("Search(alpha): %v", err)
		}
		for _, h := range alphaHits {
			if h.ID == "mem-beta" || h.AgentID != "alpha" {
				t.Fatalf("alpha received non-alpha persona vector hit: %+v", h)
			}
		}

		sharedHits, err := service.Search(t.Context(), &SearchRequest{
			OwnerID:        principal.OwnerID,
			PrincipalClass: principal.ClassOwner,
			AgentIDs:       []string{"shared-a", "shared-b"},
			Query:          "secretive synonym query",
			Now:            now,
			Limit:          10,
		})
		if err != nil {
			t.Fatalf("Search(shared): %v", err)
		}
		if len(sharedHits) != 1 || sharedHits[0].ID != "mem-shared-b" {
			t.Fatalf("sharedHits = %+v, want [mem-shared-b]", sharedHits)
		}

		boundedHits, err := service.Search(t.Context(), &SearchRequest{
			OwnerID:        principal.OwnerID,
			PrincipalClass: principal.ClassOwner,
			AgentIDs:       []string{"alpha"},
			SessionID:      "sess-alpha",
			Query:          "secretive synonym query",
			After:          boundary,
			Before:         boundary.Add(200 * time.Millisecond),
			Now:            now,
			Limit:          10,
		})
		if err != nil {
			t.Fatalf("Search(bounded): %v", err)
		}
		if len(boundedHits) != 1 || boundedHits[0].ID != "mem-sub-inside" {
			t.Fatalf("boundedHits = %+v, want only mem-sub-inside within sub-second RFC3339Nano window", boundedHits)
		}

		vecRes, err := store.SearchVectors(t.Context(), &VectorQuery{
			OwnerID:     principal.OwnerID,
			AgentIDs:    []string{"alpha"},
			Fingerprint: "fp-scope",
			Dimensions:  4,
			QueryVector: []float32{1, 0, 0, 0},
			Now:         now,
			Limit:       10,
			MaxScanRows: 2,
		})
		if err != nil {
			t.Fatalf("SearchVectors(cap): %v", err)
		}
		if vecRes.ScannedRows != 2 || len(vecRes.Hits) > 2 {
			t.Fatalf("SearchVectors(cap=2) scanned=%d hits=%d, want scanned=2", vecRes.ScannedRows, len(vecRes.Hits))
		}
	})
}

func TestMemoryEmbeddingRespectsOptInSecretsPublicQueriesFingerprintAndFallback(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

	t.Run("disabled opt-in makes zero embedder calls", func(t *testing.T) {
		db, store := newTestSQLiteStore(t)
		seedMemorySessionWithAgent(t, db, "sess-off", principal.OwnerID, "main")
		embedder := &deterministicEmbedder{}
		service, err := NewService(store, nil, &ServiceOptions{
			EmbeddingEnabled: false,
			Embedder:         embedder,
			Embedding:        &EmbeddingSettings{Fingerprint: "fp-off", Dimensions: 4},
			Vectors:          store,
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		seedHybridDoc(t, store, "mem-off", principal.OwnerID, "sess-off", "hello world", 1, now.Add(-time.Hour), false)
		if embedded, err := service.BackfillEmbeddingsOnce(t.Context(), 10); err != nil || embedded != 0 {
			t.Fatalf("BackfillEmbeddingsOnce(disabled) = (%d, %v), want (0, nil)", embedded, err)
		}
		_, mode, err := service.SearchWithMode(t.Context(), &SearchRequest{
			OwnerID:        principal.OwnerID,
			PrincipalClass: principal.ClassOwner,
			Query:          "hello",
			Now:            now,
			Limit:          5,
		})
		if err != nil {
			t.Fatalf("SearchWithMode: %v", err)
		}
		if mode != ModeLexical {
			t.Fatalf("mode = %q, want %q", mode, ModeLexical)
		}
		if len(embedder.callList()) != 0 {
			t.Fatalf("disabled service invoked embedder: %+v", embedder.callList())
		}
	})

	t.Run("vault secrets, credential patterns, and unresolved secret refs fail closed before Embed", func(t *testing.T) {
		db, store := newTestSQLiteStore(t)
		seedMemorySessionWithAgent(t, db, "sess-sec", principal.OwnerID, "main")
		vault := secret.New()
		vault.Set("telemetry_credential", "vault-canary-secret-99")
		embedder := &deterministicEmbedder{
			vectors: map[string][]float32{
				"clean document for embedding": {1, 0, 0, 0},
				"clean query":                  {1, 0, 0, 0},
			},
		}
		service, err := NewService(store, vault, &ServiceOptions{
			EmbeddingEnabled: true,
			Embedder:         embedder,
			Embedding:        &EmbeddingSettings{Fingerprint: "fp-sec", Dimensions: 4},
			SecretSource:     staticSecretSource{screen: vault},
			Vectors:          store,
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}

		awsKey := "AKIA" + "ABCDEFGHIJKLMNOP"
		seedHybridDoc(t, store, "mem-clean", principal.OwnerID, "sess-sec", "clean document for embedding", 1, now.Add(-3*time.Hour), false)
		seedHybridDoc(t, store, "mem-vault", principal.OwnerID, "sess-sec", "contains vault-canary-secret-99 inside", 2, now.Add(-2*time.Hour), false)
		seedHybridDoc(t, store, "mem-pattern", principal.OwnerID, "sess-sec", "my key is "+awsKey+" ok", 3, now.Add(-time.Hour), false)

		embedded, err := service.BackfillEmbeddingsOnce(t.Context(), 50)
		if err != nil {
			t.Fatalf("BackfillEmbeddingsOnce: %v", err)
		}
		if embedded != 1 {
			t.Fatalf("embedded = %d, want 1 (only clean doc)", embedded)
		}
		if embedder.sentContains("vault-canary-secret-99") || embedder.sentContains(awsKey) {
			t.Fatalf("secret or credential pattern was sent to embedder: %+v", embedder.callList())
		}

		embedder.resetCalls()
		for _, badQuery := range []string{"find vault-canary-secret-99", "lookup " + awsKey} {
			_, mode, err := service.SearchWithMode(t.Context(), &SearchRequest{
				OwnerID:        principal.OwnerID,
				PrincipalClass: principal.ClassOwner,
				Query:          badQuery,
				Now:            now,
				Limit:          5,
			})
			if err != nil {
				t.Fatalf("SearchWithMode(%q): %v", badQuery, err)
			}
			if mode != ModeLexical {
				t.Fatalf("SearchWithMode(%q) mode = %q, want %q", badQuery, mode, ModeLexical)
			}
		}
		if len(embedder.callList()) != 0 {
			t.Fatalf("sensitive query was sent to embedder: %+v", embedder.callList())
		}

		failClosedSvc, err := NewService(store, vault, &ServiceOptions{
			EmbeddingEnabled: true,
			Embedder:         embedder,
			Embedding:        &EmbeddingSettings{Fingerprint: "fp-sec", Dimensions: 4},
			SecretSource:     staticSecretSource{err: errors.New("unresolved secret ref")},
			Vectors:          store,
		})
		if err != nil {
			t.Fatalf("NewService(failClosed): %v", err)
		}
		seedHybridDoc(t, store, "mem-clean-2", principal.OwnerID, "sess-sec", "another clean doc", 4, now.Add(-10*time.Minute), false)
		if _, err := failClosedSvc.BackfillEmbeddingsOnce(t.Context(), 10); err == nil {
			t.Fatal("BackfillEmbeddingsOnce succeeded despite unresolved secret ref")
		}
		_, mode, err := failClosedSvc.SearchWithMode(t.Context(), &SearchRequest{
			OwnerID:        principal.OwnerID,
			PrincipalClass: principal.ClassOwner,
			Query:          "clean query",
			Now:            now,
			Limit:          5,
		})
		if err != nil {
			t.Fatalf("SearchWithMode(unresolved): %v", err)
		}
		if mode != ModeLexical {
			t.Fatalf("unresolved secret ref mode = %q, want %q", mode, ModeLexical)
		}
		if len(embedder.callList()) != 0 {
			t.Fatalf("unresolved secret ref still called embedder: %+v", embedder.callList())
		}
	})

	t.Run("public_queries gates ClassPublic queries and non-owner documents", func(t *testing.T) {
		db, store := newTestSQLiteStore(t)
		seedMemorySessionWithAgent(t, db, "sess-owner", principal.OwnerID, "main")
		seedMemorySessionWithAgent(t, db, "sess-guest", "discord:guest-1", "main")

		embedder := &deterministicEmbedder{
			vectors: map[string][]float32{
				"owner note":  {1, 0, 0, 0},
				"guest note":  {1, 0, 0, 0},
				"guest query": {1, 0, 0, 0},
			},
		}
		seedHybridDoc(t, store, "mem-owner", principal.OwnerID, "sess-owner", "owner note", 1, now.Add(-2*time.Hour), false)
		seedHybridDoc(t, store, "mem-guest", "discord:guest-1", "sess-guest", "guest note", 1, now.Add(-time.Hour), false)

		svcClosed, err := NewService(store, nil, &ServiceOptions{
			EmbeddingEnabled: true,
			Embedder:         embedder,
			Embedding:        &EmbeddingSettings{Fingerprint: "fp-pub", Dimensions: 4, PublicQueries: false},
			Vectors:          store,
		})
		if err != nil {
			t.Fatalf("NewService(closed): %v", err)
		}
		embedded, err := svcClosed.BackfillEmbeddingsOnce(t.Context(), 50)
		if err != nil || embedded != 1 {
			t.Fatalf("Backfill(public_queries=false) = (%d, %v), want (1, nil) owner-only", embedded, err)
		}
		if embedder.sentContains("guest note") {
			t.Fatal("non-owner document sent to embedder when public_queries=false")
		}
		embedder.resetCalls()

		_, mode, err := svcClosed.SearchWithMode(t.Context(), &SearchRequest{
			OwnerID:        principal.OwnerID,
			PrincipalClass: principal.ClassPublic,
			Query:          "guest query",
			Now:            now,
			Limit:          5,
		})
		if err != nil {
			t.Fatalf("SearchWithMode(ClassPublic, closed): %v", err)
		}
		if mode != ModeLexical || embedder.queryCallCount() != 0 {
			t.Fatalf("ClassPublic with public_queries=false got mode=%q calls=%d, want lexical/0", mode, embedder.queryCallCount())
		}

		for _, emptyOrUnknown := range []principal.Class{"", principal.Class("unknown")} {
			_, mode, err := svcClosed.SearchWithMode(t.Context(), &SearchRequest{
				OwnerID:        principal.OwnerID,
				PrincipalClass: emptyOrUnknown,
				Query:          "guest query",
				Now:            now,
				Limit:          5,
			})
			if err != nil {
				t.Fatalf("SearchWithMode(%q): %v", emptyOrUnknown, err)
			}
			if mode != ModeLexical || embedder.queryCallCount() != 0 {
				t.Fatalf("PrincipalClass=%q with public_queries=false got mode=%q calls=%d, want lexical/0", emptyOrUnknown, mode, embedder.queryCallCount())
			}
		}

		ops, err := NewOperations(&OperationDeps{
			Service: svcClosed,
			Scopes:  staticScopes{"main": {"main"}},
			Now:     func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("NewOperations: %v", err)
		}
		var searchHandle operation.Handler
		for _, b := range ops.Bindings() {
			if b.ID == operationSearch {
				searchHandle = b.Handle
			}
		}
		chatRes, err := searchHandle(t.Context(), &operation.Call{
			Caller: operation.Caller{Surface: operation.SurfaceChat, PrincipalID: principal.OwnerID},
			Input:  json.RawMessage(`{"query":"guest query","limit":5}`),
		})
		if err != nil {
			t.Fatalf("memory.search(chat empty class): %v", err)
		}
		var chatPayload struct {
			Mode string `json:"mode"`
		}
		if err := json.Unmarshal(chatRes.Data, &chatPayload); err != nil {
			t.Fatalf("unmarshal chat search: %v", err)
		}
		if chatPayload.Mode != ModeLexical || embedder.queryCallCount() != 0 {
			t.Fatalf("SurfaceChat with empty Caller.Class got mode=%q calls=%d, want lexical/0", chatPayload.Mode, embedder.queryCallCount())
		}

		cliRes, err := searchHandle(t.Context(), &operation.Call{
			Caller: operation.Caller{Surface: operation.SurfaceCLI, PrincipalID: principal.OwnerID},
			Input:  json.RawMessage(`{"query":"guest query","limit":5}`),
		})
		if err != nil {
			t.Fatalf("memory.search(cli empty class): %v", err)
		}
		var cliPayload struct {
			Mode string `json:"mode"`
		}
		if err := json.Unmarshal(cliRes.Data, &cliPayload); err != nil {
			t.Fatalf("unmarshal cli search: %v", err)
		}
		if cliPayload.Mode != ModeHybrid || embedder.queryCallCount() != 1 {
			t.Fatalf("SurfaceCLI with empty Caller.Class got mode=%q calls=%d, want hybrid/1", cliPayload.Mode, embedder.queryCallCount())
		}
		embedder.resetCalls()

		svcOpen, err := NewService(store, nil, &ServiceOptions{
			EmbeddingEnabled: true,
			Embedder:         embedder,
			Embedding:        &EmbeddingSettings{Fingerprint: "fp-pub", Dimensions: 4, PublicQueries: true},
			Vectors:          store,
		})
		if err != nil {
			t.Fatalf("NewService(open): %v", err)
		}
		embedded, err = svcOpen.BackfillEmbeddingsOnce(t.Context(), 50)
		if err != nil || embedded != 1 {
			t.Fatalf("Backfill(public_queries=true) = (%d, %v), want (1, nil) for guest doc", embedded, err)
		}
		embedder.resetCalls()

		hits, mode, err := svcOpen.SearchWithMode(t.Context(), &SearchRequest{
			OwnerID:        "discord:guest-1",
			PrincipalClass: principal.ClassPublic,
			Query:          "guest query",
			Now:            now,
			Limit:          5,
		})
		if err != nil {
			t.Fatalf("SearchWithMode(ClassPublic, open): %v", err)
		}
		if mode != ModeHybrid || len(hits) != 1 || hits[0].ID != "mem-guest" || embedder.queryCallCount() != 1 {
			t.Fatalf("ClassPublic with public_queries=true got mode=%q hits=%+v calls=%d", mode, hits, embedder.queryCallCount())
		}
	})

	t.Run("provider error, timeout, and invalid dimension fall back to lexical_fallback", func(t *testing.T) {
		db, store := newTestSQLiteStore(t)
		seedMemorySessionWithAgent(t, db, "sess-fb", principal.OwnerID, "main")
		seedHybridDoc(t, store, "mem-fb", principal.OwnerID, "sess-fb", "postgres migration runbook", 1, now.Add(-time.Hour), false)
		if err := store.UpsertEmbeddings(t.Context(), "fp-fb", []EmbeddingWrite{{
			DocumentID:    "mem-fb",
			ContentSHA256: contentSHA256("postgres migration runbook"),
			Vector:        []float32{1, 0, 0, 0},
			CreatedAt:     now,
		}}); err != nil {
			t.Fatalf("UpsertEmbeddings: %v", err)
		}

		cases := []struct {
			name     string
			embedder *deterministicEmbedder
			timeout  time.Duration
		}{
			{"provider error", &deterministicEmbedder{err: errors.New("upstream 503")}, time.Second},
			{"timeout", &deterministicEmbedder{delay: 200 * time.Millisecond}, 20 * time.Millisecond},
			{"invalid dimension", &deterministicEmbedder{vectors: map[string][]float32{"postgres": {1, 0}}}, time.Second},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				service, err := NewService(store, nil, &ServiceOptions{
					EmbeddingEnabled: true,
					Embedder:         tc.embedder,
					Embedding:        &EmbeddingSettings{Fingerprint: "fp-fb", Dimensions: 4, QueryTimeout: tc.timeout},
					Vectors:          store,
				})
				if err != nil {
					t.Fatalf("NewService: %v", err)
				}
				hits, mode, err := service.SearchWithMode(t.Context(), &SearchRequest{
					OwnerID:        principal.OwnerID,
					PrincipalClass: principal.ClassOwner,
					Query:          "postgres",
					Now:            now,
					Limit:          5,
				})
				if err != nil {
					t.Fatalf("SearchWithMode: %v", err)
				}
				if mode != ModeLexicalFallback {
					t.Fatalf("mode = %q, want %q", mode, ModeLexicalFallback)
				}
				if len(hits) != 1 || hits[0].ID != "mem-fb" {
					t.Fatalf("hits = %+v, want lexical fallback hit mem-fb", hits)
				}
			})
		}
	})

	t.Run("fingerprint isolation, backfill re-embed, cascade deletes, strict forget, and health check", func(t *testing.T) {
		db, store := newTestSQLiteStore(t)
		seedMemorySessionWithAgent(t, db, "sess-fp", principal.OwnerID, "main")
		seedMemorySessionWithAgent(t, db, "sess-del", principal.OwnerID, "main")

		seedHybridDoc(t, store, "mem-fp-1", principal.OwnerID, "sess-fp", "terbang ke Jepang", 1, now.Add(-2*time.Hour), false)
		seedHybridDoc(t, store, "mem-fp-2", principal.OwnerID, "sess-fp", "catatan rapat produk", 2, now.Add(-time.Hour), false)
		seedHybridDoc(t, store, "mem-del-1", principal.OwnerID, "sess-del", "sesi akan dihapus", 1, now.Add(-time.Hour), false)

		if err := store.UpsertEmbeddings(t.Context(), "fp-old", []EmbeddingWrite{{
			DocumentID:    "mem-fp-1",
			ContentSHA256: contentSHA256("terbang ke Jepang"),
			Vector:        []float32{1, 0, 0, 0},
			CreatedAt:     now.Add(-time.Hour),
		}}); err != nil {
			t.Fatalf("UpsertEmbeddings(old): %v", err)
		}
		if err := store.UpsertEmbeddings(t.Context(), "fp-new", []EmbeddingWrite{{
			DocumentID:    "mem-fp-2",
			ContentSHA256: contentSHA256("catatan rapat produk"),
			Vector:        []float32{0, 1, 0, 0},
			CreatedAt:     now.Add(-time.Hour),
		}}); err != nil {
			t.Fatalf("UpsertEmbeddings(new): %v", err)
		}

		embedder := &deterministicEmbedder{
			vectors: map[string][]float32{
				"pesawat ke Tokyo":    {1, 0, 0, 0},
				"terbang ke Jepang":   {1, 0, 0, 0},
				"sesi akan dihapus":   {0, 0, 1, 0},
				"catatan rapat":       {0, 1, 0, 0},
				"terbang ke Jepang 2": {1, 0, 0, 0},
			},
		}
		sigStore := &signalingVectorStore{
			SQLiteStore: store,
			upserted:    make(chan struct{}, 4),
		}
		service, err := NewService(store, nil, &ServiceOptions{
			EmbeddingEnabled: true,
			Embedder:         embedder,
			Embedding:        &EmbeddingSettings{Fingerprint: "fp-new", Dimensions: 4, BatchSize: 16},
			Vectors:          sigStore,
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}

		hits, mode, err := service.SearchWithMode(t.Context(), &SearchRequest{
			OwnerID:        principal.OwnerID,
			PrincipalClass: principal.ClassOwner,
			SessionID:      "sess-fp",
			Query:          "pesawat ke Tokyo",
			Now:            now,
			Limit:          5,
		})
		if err != nil {
			t.Fatalf("SearchWithMode(stale fp): %v", err)
		}
		if mode != ModeHybrid {
			t.Fatalf("mode = %q, want %q", mode, ModeHybrid)
		}
		for _, h := range hits {
			if h.ID == "mem-fp-1" {
				t.Fatalf("stale-fingerprint vector mem-fp-1 was scored under fp-new: %+v", hits)
			}
		}

		hc := &memoryEmbeddingHealth{
			store:     store,
			settings:  &EmbeddingSettings{Fingerprint: "fp-new", Dimensions: 4},
			available: true,
		}
		findings := hc.Check(t.Context())
		if len(findings) != 1 || findings[0].Code != "reembed_required" || findings[0].Status != health.StatusDegraded {
			t.Fatalf("health findings before backfill = %+v, want reembed_required", findings)
		}

		runCtx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			service.RunEmbeddingBackfill(runCtx, time.Hour)
		}()
		select {
		case <-sigStore.upserted:
		case <-time.After(2 * time.Second):
			cancel()
			<-done
			t.Fatal("timed out waiting for memory embedding backfill upsert")
		}
		cancel()
		<-done

		cov, err := store.EmbeddingCoverage(t.Context(), "fp-new", false, now)
		if err != nil || cov.Stale != 0 || cov.Embedded != 3 {
			t.Fatalf("coverage after backfill = (%+v, %v), want embedded=3 stale=0", cov, err)
		}

		findings = hc.Check(t.Context())
		if len(findings) != 1 || findings[0].Code != "ok" || findings[0].Status != health.StatusUp {
			t.Fatalf("health findings after backfill = %+v, want ok", findings)
		}

		hits, mode, err = service.SearchWithMode(t.Context(), &SearchRequest{
			OwnerID:        principal.OwnerID,
			PrincipalClass: principal.ClassOwner,
			SessionID:      "sess-fp",
			Query:          "pesawat ke Tokyo",
			Now:            now,
			Limit:          5,
		})
		if err != nil {
			t.Fatalf("SearchWithMode(re-embedded): %v", err)
		}
		if mode != ModeHybrid || len(hits) != 1 || hits[0].ID != "mem-fp-1" {
			t.Fatalf("post-backfill search = (%+v, %q), want mem-fp-1 hybrid", hits, mode)
		}

		embedder.resetCalls()
		preview, err := service.PreviewForgetByQuery(t.Context(), &DocumentQuery{
			OwnerID: principal.OwnerID,
			Terms:   "pesawat ke Tokyo",
			Limit:   10,
		})
		if err != nil {
			t.Fatalf("PreviewForgetByQuery: %v", err)
		}
		if len(preview) != 0 {
			t.Fatalf("PreviewForgetByQuery matched via vector instead of strict AND: %+v", preview)
		}
		if len(embedder.callList()) != 0 {
			t.Fatalf("PreviewForgetByQuery called embedder: %+v", embedder.callList())
		}

		exactPreview, err := service.PreviewForgetByQuery(t.Context(), &DocumentQuery{
			OwnerID: principal.OwnerID,
			Terms:   "terbang Jepang",
			Limit:   10,
		})
		if err != nil || len(exactPreview) != 1 {
			t.Fatalf("PreviewForgetByQuery(exact) = (%+v, %v)", exactPreview, err)
		}
		if _, err := service.Forget(t.Context(), principal.OwnerID, exactPreview); err != nil {
			t.Fatalf("Forget: %v", err)
		}
		if got := countMemoryEmbeddings(t, db); got != 2 {
			t.Fatalf("memory_embedding rows after forget = %d, want 2 (ON DELETE CASCADE)", got)
		}

		expiredAt := now.Add(-time.Minute)
		clean := false
		if _, err := store.UpsertDocument(t.Context(), &StoredDocument{
			ID: "mem-exp-sum", OwnerID: principal.OwnerID, SessionID: "sess-fp", Kind: KindSummary,
			FromSequence: 1, ToSequence: 2, Content: "ringkasan lama", TrustLabel: "derived_untrusted",
			Tainted: &clean, PromptVersion: "v1", CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: &expiredAt,
		}); err != nil {
			t.Fatalf("UpsertDocument(expired summary): %v", err)
		}
		if err := store.UpsertEmbeddings(t.Context(), "fp-new", []EmbeddingWrite{{
			DocumentID: "mem-exp-sum", ContentSHA256: contentSHA256("ringkasan lama"), Vector: []float32{0, 0, 0, 1}, CreatedAt: now.Add(-2 * time.Hour),
		}}); err != nil {
			t.Fatalf("UpsertEmbeddings(expired summary): %v", err)
		}
		if countMemoryEmbeddings(t, db) != 3 {
			t.Fatalf("expected 3 embeddings before purge")
		}
		if _, err := service.PurgeExpired(t.Context(), now, 10); err != nil {
			t.Fatalf("PurgeExpired: %v", err)
		}
		if got := countMemoryEmbeddings(t, db); got != 2 {
			t.Fatalf("memory_embedding rows after purge = %d, want 2", got)
		}

		if _, err := db.ExecContext(t.Context(), `DELETE FROM session WHERE id = 'sess-del'`); err != nil {
			t.Fatalf("delete session: %v", err)
		}
		if got := countMemoryEmbeddings(t, db); got != 1 {
			t.Fatalf("memory_embedding rows after session delete = %d, want 1", got)
		}
	})

	t.Run("ProjectBatch releases projectMu before calling Embed", func(t *testing.T) {
		db, store := newTestSQLiteStore(t)
		seedMemorySessionWithAgent(t, db, "sess-mu", principal.OwnerID, "main")

		var service *Service
		lockFree := false
		embedder := &deterministicEmbedder{
			vectors: map[string][]float32{
				"post lock document": {1, 0, 0, 0},
			},
			onEmbed: func(role embedding.Role, _ []string) {
				if role == embedding.RoleDocument && service.projectMu.TryLock() {
					lockFree = true
					service.projectMu.Unlock()
				}
			},
		}
		var err error
		service, err = NewService(store, nil, &ServiceOptions{
			EmbeddingEnabled: true,
			Embedder:         embedder,
			Embedding:        &EmbeddingSettings{Fingerprint: "fp-mu", Dimensions: 4},
			Vectors:          store,
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		res, err := service.ProjectBatch(t.Context(), principal.OwnerID, []*Event{{
			ID:        "evt-mu-1",
			SessionID: "sess-mu",
			Sequence:  1,
			Author:    "user",
			Kind:      "adk_event",
			Payload:   json.RawMessage(`{"text":"post lock document"}`),
			CreatedAt: now,
		}})
		if err != nil || res.Indexed != 1 {
			t.Fatalf("ProjectBatch = (%+v, %v), want indexed=1", res, err)
		}
		if !lockFree {
			t.Fatal("ProjectBatch held projectMu while invoking Embed")
		}
		if got := countMemoryEmbeddings(t, db); got != 1 {
			t.Fatalf("memory_embedding count = %d, want 1", got)
		}
	})
}
