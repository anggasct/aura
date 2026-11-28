package profile

import (
	stdcontext "context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/embedding"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/secret"
)

type profileTestEmbedder struct {
	mu      sync.Mutex
	vectors map[string][]float32
	calls   [][]string
	roles   []embedding.Role
	err     error
	delay   time.Duration
}

func (e *profileTestEmbedder) Embed(ctx stdcontext.Context, role embedding.Role, texts []string) ([][]float32, error) {
	e.mu.Lock()
	e.calls = append(e.calls, slices.Clone(texts))
	e.roles = append(e.roles, role)
	err := e.err
	delay := e.delay
	vecs := e.vectors
	e.mu.Unlock()
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

func (e *profileTestEmbedder) sentContains(needle string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, batch := range e.calls {
		for _, text := range batch {
			if strings.Contains(text, needle) {
				return true
			}
		}
	}
	return false
}

func countProfileEmbeddings(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM profile_embedding`).Scan(&n); err != nil {
		t.Fatalf("count profile_embedding: %v", err)
	}
	return n
}

func hasProfileEmbedding(t *testing.T, db *sql.DB, factID string) bool {
	t.Helper()
	var exists bool
	if err := db.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM profile_embedding WHERE fact_id = ?)`, factID).Scan(&exists); err != nil {
		t.Fatalf("check profile_embedding(%s): %v", factID, err)
	}
	return exists
}

func TestProfileHybridSearchMatchesVerifiedActiveFactsOnly(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

	t.Run("backfill runs without extractor and matches verified active facts semantically", func(t *testing.T) {
		_, db := newTestSQLiteStore(t)
		embedder := &profileTestEmbedder{
			vectors: map[string][]float32{
				"destination: terbang ke Jepang":  {1, 0, 0, 0},
				"pesawat ke Tokyo":                {1, 0, 0, 0},
				"unverified_key: terbang ke Bali": {1, 0, 0, 0},
				"candidate_key: terbang ke Osaka": {1, 0, 0, 0},
				"rejected_key: terbang ke Seoul":  {1, 0, 0, 0},
				"expired_key: terbang ke Kyoto":   {1, 0, 0, 0},
			},
		}
		cfg := Default()
		cfg.EmbeddingEnabled = true
		vault := secret.New()
		vault.Set("secret_val", "top-secret-profile-99")
		mod, err := New(t.Context(), &Deps{
			Config:   cfg,
			DB:       db,
			Scanner:  vault,
			Embedder: embedder,
			Embedding: &EmbeddingSettings{
				Fingerprint: "fp-prof-v1",
				Dimensions:  4,
				BatchSize:   16,
			},
		})
		if err != nil {
			t.Fatalf("profile.New: %v", err)
		}
		if mod.Extractor() != nil {
			t.Fatal("expected extractor == nil when no models are wired")
		}

		rawStore, err := NewSQLiteStore(db)
		if err != nil {
			t.Fatalf("NewSQLiteStore: %v", err)
		}
		past := now.Add(-time.Hour)
		facts := []Fact{
			{
				ID: "pf-verified", OwnerID: principal.OwnerID, Category: "preference",
				Key: "destination", Value: "terbang ke Jepang", ValueDigest: ValueDigest("terbang ke Jepang"),
				Status: StatusActive, Origin: OriginOwner, OwnerVerified: true,
				Confidence: 1.0, ConfidencePolicy: ConfidencePolicyV1, CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour),
			},
			{
				ID: "pf-unverified", OwnerID: principal.OwnerID, Category: "preference",
				Key: "unverified_key", Value: "terbang ke Bali", ValueDigest: ValueDigest("terbang ke Bali"),
				Status: StatusActive, Origin: OriginDerived, OwnerVerified: false,
				Confidence: 0.9, ConfidencePolicy: ConfidencePolicyV1, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
			},
			{
				ID: "pf-candidate", OwnerID: principal.OwnerID, Category: "preference",
				Key: "candidate_key", Value: "terbang ke Osaka", ValueDigest: ValueDigest("terbang ke Osaka"),
				Status: StatusCandidate, Origin: OriginDerived, OwnerVerified: false,
				Confidence: 0.6, ConfidencePolicy: ConfidencePolicyV1, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
			},
			{
				ID: "pf-rejected", OwnerID: principal.OwnerID, Category: "preference",
				Key: "rejected_key", Value: "terbang ke Seoul", ValueDigest: ValueDigest("terbang ke Seoul"),
				Status: StatusRejected, Origin: OriginOwner, OwnerVerified: true,
				Confidence: 1.0, ConfidencePolicy: ConfidencePolicyV1, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
			},
			{
				ID: "pf-expired", OwnerID: principal.OwnerID, Category: "preference",
				Key: "expired_key", Value: "terbang ke Kyoto", ValueDigest: ValueDigest("terbang ke Kyoto"),
				Status: StatusActive, Origin: OriginOwner, OwnerVerified: true, ExpiresAt: &past,
				Confidence: 1.0, ConfidencePolicy: ConfidencePolicyV1, CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now.Add(-3 * time.Hour),
			},
		}
		for i := range facts {
			if err := rawStore.InsertFact(t.Context(), &facts[i]); err != nil {
				t.Fatalf("InsertFact(%s): %v", facts[i].ID, err)
			}
		}

		runCtx, cancel := stdcontext.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			mod.Run(runCtx)
		}()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if countProfileEmbeddings(t, db) >= 1 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		<-done

		if !hasProfileEmbedding(t, db, "pf-verified") || countProfileEmbeddings(t, db) != 1 {
			t.Fatalf("profile_embedding count=%d pf-verified=%t, want only pf-verified", countProfileEmbeddings(t, db), hasProfileEmbedding(t, db, "pf-verified"))
		}
		for _, forbidden := range []string{"terbang ke Bali", "terbang ke Osaka", "terbang ke Seoul", "terbang ke Kyoto"} {
			if embedder.sentContains(forbidden) {
				t.Fatalf("non-verified/inactive/expired fact %q was sent to embedder", forbidden)
			}
		}

		part, err := mod.Service().Retrieve(t.Context(), &RetrieveQuery{
			OwnerID:      principal.OwnerID,
			Query:        "pesawat ke Tokyo",
			MaxFacts:     5,
			MaxTokens:    512,
			VerifiedOnly: true,
		}, now)
		if err != nil {
			t.Fatalf("Retrieve: %v", err)
		}
		if len(part.Facts) != 1 || part.Facts[0].ID != "pf-verified" {
			t.Fatalf("Retrieve facts = %+v, want [pf-verified]", part.Facts)
		}

		checks := mod.HealthChecks()
		var embedCheck health.Checker
		for _, c := range checks {
			if c.ID == ComponentProfileEmbedding {
				embedCheck = c.Checker
			}
		}
		if embedCheck == nil {
			t.Fatal("missing profile_embedding health check when embedding_enabled=true")
		}
		findings := embedCheck.Check(t.Context())
		if len(findings) != 1 || findings[0].Code != "ok" || findings[0].Status != health.StatusUp {
			t.Fatalf("profile_embedding health = %+v, want ok", findings)
		}
	})

	t.Run("soft delete, reject, expire, supersession, and UpdateFact remove embeddings immediately via trigger", func(t *testing.T) {
		_, db := newTestSQLiteStore(t)
		embedder := &profileTestEmbedder{
			vectors: map[string][]float32{
				"editor: neovim":        {1, 0, 0, 0},
				"editor: helix":         {0, 1, 0, 0},
				"shell: zsh":            {0, 0, 1, 0},
				"browser: firefox":      {0.5, 0.5, 0.5, 0.5},
				"temp_pref: dark theme": {0, 0, 0, 1},
				"color: cobalt blue":    {1, 0, 0, 0},
				"color: emerald green":  {0, 1, 0, 0},
			},
		}
		store, err := NewSQLiteStoreWithEmbedding(db, &EmbeddingOptions{
			Enabled:  true,
			Embedder: embedder,
			Settings: &EmbeddingSettings{Fingerprint: "fp-trig", Dimensions: 4},
		})
		if err != nil {
			t.Fatalf("NewSQLiteStoreWithEmbedding: %v", err)
		}
		svc, err := NewService(store, ServiceOptions{MinConfidence: 0.7})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}

		editorOld, err := svc.SetFact(t.Context(), principal.OwnerID, "tool", "editor", "neovim", nil, now)
		if err != nil || !hasProfileEmbedding(t, db, editorOld.ID) {
			t.Fatalf("SetFact(editor=neovim) err=%v embedded=%t", err, hasProfileEmbedding(t, db, editorOld.ID))
		}
		editorNew, err := svc.SetFact(t.Context(), principal.OwnerID, "tool", "editor", "helix", nil, now.Add(time.Second))
		if err != nil {
			t.Fatalf("SetFact(editor=helix supersession): %v", err)
		}
		if hasProfileEmbedding(t, db, editorOld.ID) {
			t.Fatal("superseded fact kept orphan profile_embedding row")
		}
		if !hasProfileEmbedding(t, db, editorNew.ID) {
			t.Fatal("superseding fact was not embedded")
		}

		if err := svc.DeleteFact(t.Context(), principal.OwnerID, editorNew.ID, now.Add(2*time.Second)); err != nil {
			t.Fatalf("DeleteFact: %v", err)
		}
		if hasProfileEmbedding(t, db, editorNew.ID) {
			t.Fatal("DeleteFact left orphan profile_embedding row")
		}

		shellFact, err := svc.SetFact(t.Context(), principal.OwnerID, "tool", "shell", "zsh", nil, now)
		if err != nil || !hasProfileEmbedding(t, db, shellFact.ID) {
			t.Fatalf("SetFact(shell) err=%v embedded=%t", err, hasProfileEmbedding(t, db, shellFact.ID))
		}
		if err := svc.RejectFact(t.Context(), principal.OwnerID, shellFact.ID, now.Add(time.Second)); err != nil {
			t.Fatalf("RejectFact: %v", err)
		}
		if hasProfileEmbedding(t, db, shellFact.ID) {
			t.Fatal("RejectFact left orphan profile_embedding row")
		}

		expAt := now.Add(time.Minute)
		tempFact, err := svc.SetFact(t.Context(), principal.OwnerID, "preference", "temp_pref", "dark theme", &expAt, now)
		if err != nil || !hasProfileEmbedding(t, db, tempFact.ID) {
			t.Fatalf("SetFact(temp_pref) err=%v embedded=%t", err, hasProfileEmbedding(t, db, tempFact.ID))
		}
		if _, err := svc.ExpireFacts(t.Context(), now.Add(2*time.Minute)); err != nil {
			t.Fatalf("ExpireFacts: %v", err)
		}
		if hasProfileEmbedding(t, db, tempFact.ID) {
			t.Fatal("ExpireFacts left orphan profile_embedding row")
		}

		colorFact, err := svc.SetFact(t.Context(), principal.OwnerID, "preference", "color", "cobalt blue", nil, now)
		if err != nil || !hasProfileEmbedding(t, db, colorFact.ID) {
			t.Fatalf("SetFact(color) err=%v embedded=%t", err, hasProfileEmbedding(t, db, colorFact.ID))
		}
		colorFact.Value = "emerald green"
		colorFact.ValueDigest = ValueDigest("emerald green")
		colorFact.UpdatedAt = now.Add(time.Second)
		if err := store.UpdateFact(t.Context(), &colorFact); err != nil {
			t.Fatalf("UpdateFact: %v", err)
		}
		if hasProfileEmbedding(t, db, colorFact.ID) {
			t.Fatal("UpdateFact changing fact_value left stale profile_embedding row")
		}
		if got := countProfileEmbeddings(t, db); got != 0 {
			t.Fatalf("total orphan profile_embedding rows = %d, want 0", got)
		}
	})

	t.Run("lexical fallback on embed failure and stale fingerprint health warning", func(t *testing.T) {
		_, db := newTestSQLiteStore(t)
		failing := &profileTestEmbedder{err: errors.New("provider unavailable")}
		store, err := NewSQLiteStoreWithEmbedding(db, &EmbeddingOptions{
			Enabled:  true,
			Embedder: failing,
			Settings: &EmbeddingSettings{Fingerprint: "fp-fb", Dimensions: 4},
		})
		if err != nil {
			t.Fatalf("NewSQLiteStoreWithEmbedding: %v", err)
		}
		seedActiveProfileFact(t, store, "pf-go", "language", "backend", "golang systems programming", true, now.Add(-time.Hour))
		if err := store.upsertFactEmbeddings(t.Context(), "fp-fb", []profileEmbeddingWrite{{
			FactID:        "pf-go",
			ContentSHA256: factContentSHA256(factEmbedText("backend", "golang systems programming")),
			Vector:        []float32{1, 0, 0, 0},
			CreatedAt:     now,
		}}); err != nil {
			t.Fatalf("upsertFactEmbeddings: %v", err)
		}

		facts, err := store.SearchFacts(t.Context(), "owner-1", "", "golang systems", true, 5, now)
		if err != nil {
			t.Fatalf("SearchFacts(fallback): %v", err)
		}
		if len(facts) != 1 || facts[0].ID != "pf-go" {
			t.Fatalf("SearchFacts(fallback) = %+v, want lexical hit pf-go", facts)
		}

		hc := &profileEmbeddingHealth{
			store:     store,
			settings:  &EmbeddingSettings{Fingerprint: "fp-other", Dimensions: 4},
			available: true,
		}
		findings := hc.Check(t.Context())
		if len(findings) != 1 || findings[0].Code != "reembed_required" || findings[0].Status != health.StatusDegraded {
			t.Fatalf("stale profile health findings = %+v, want reembed_required", findings)
		}
	})
}
