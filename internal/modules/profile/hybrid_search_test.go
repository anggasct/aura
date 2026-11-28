package profile

import (
	stdcontext "context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anggasct/aura/internal/platform/embedding"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/secret"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

type profileTestEmbedder struct {
	mu      sync.Mutex
	vectors map[string][]float32
	calls   [][]string
	roles   []embedding.Role
	err     error
	delay   time.Duration
	onEmbed func(ctx stdcontext.Context, role embedding.Role, texts []string)
}

func (e *profileTestEmbedder) Embed(ctx stdcontext.Context, role embedding.Role, texts []string) ([][]float32, error) {
	e.mu.Lock()
	e.calls = append(e.calls, slices.Clone(texts))
	e.roles = append(e.roles, role)
	err := e.err
	delay := e.delay
	vecs := e.vectors
	onEmbed := e.onEmbed
	e.mu.Unlock()
	if onEmbed != nil {
		onEmbed(ctx, role, texts)
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
		synctest.Test(t, func(t *testing.T) {
			testNow := time.Now().UTC()
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
			past := testNow.Add(-time.Hour)
			facts := []Fact{
				{
					ID: "pf-verified", OwnerID: principal.OwnerID, Category: "preference",
					Key: "destination", Value: "terbang ke Jepang", ValueDigest: ValueDigest("terbang ke Jepang"),
					Status: StatusActive, Origin: OriginOwner, OwnerVerified: true,
					Confidence: 1.0, ConfidencePolicy: ConfidencePolicyV1, CreatedAt: testNow.Add(-2 * time.Hour), UpdatedAt: testNow.Add(-2 * time.Hour),
				},
				{
					ID: "pf-unverified", OwnerID: principal.OwnerID, Category: "preference",
					Key: "unverified_key", Value: "terbang ke Bali", ValueDigest: ValueDigest("terbang ke Bali"),
					Status: StatusActive, Origin: OriginDerived, OwnerVerified: false,
					Confidence: 0.9, ConfidencePolicy: ConfidencePolicyV1, CreatedAt: testNow.Add(-time.Hour), UpdatedAt: testNow.Add(-time.Hour),
				},
				{
					ID: "pf-candidate", OwnerID: principal.OwnerID, Category: "preference",
					Key: "candidate_key", Value: "terbang ke Osaka", ValueDigest: ValueDigest("terbang ke Osaka"),
					Status: StatusCandidate, Origin: OriginDerived, OwnerVerified: false,
					Confidence: 0.6, ConfidencePolicy: ConfidencePolicyV1, CreatedAt: testNow.Add(-time.Hour), UpdatedAt: testNow.Add(-time.Hour),
				},
				{
					ID: "pf-rejected", OwnerID: principal.OwnerID, Category: "preference",
					Key: "rejected_key", Value: "terbang ke Seoul", ValueDigest: ValueDigest("terbang ke Seoul"),
					Status: StatusRejected, Origin: OriginOwner, OwnerVerified: true,
					Confidence: 1.0, ConfidencePolicy: ConfidencePolicyV1, CreatedAt: testNow.Add(-time.Hour), UpdatedAt: testNow.Add(-time.Hour),
				},
				{
					ID: "pf-expired", OwnerID: principal.OwnerID, Category: "preference",
					Key: "expired_key", Value: "terbang ke Kyoto", ValueDigest: ValueDigest("terbang ke Kyoto"),
					Status: StatusActive, Origin: OriginOwner, OwnerVerified: true, ExpiresAt: &past,
					Confidence: 1.0, ConfidencePolicy: ConfidencePolicyV1, CreatedAt: testNow.Add(-3 * time.Hour), UpdatedAt: testNow.Add(-3 * time.Hour),
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
			synctest.Wait()
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
			}, testNow)
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
		if err := store.upsertFactEmbeddings(t.Context(), "fp-trig", []profileVectorWrite{{
			FactID:        colorFact.ID,
			FactKey:       "color",
			FactValue:     "cobalt blue",
			ContentSHA256: factContentSHA256(factEmbedText("color", "cobalt blue")),
			Vector:        []float32{1, 0, 0, 0},
			CreatedAt:     now.Add(2 * time.Second),
		}}); err != nil {
			t.Fatalf("upsertFactEmbeddings(stale): %v", err)
		}
		if hasProfileEmbedding(t, db, colorFact.ID) {
			t.Fatal("stale upsertFactEmbeddings wrote vector for updated fact_value")
		}
		if got := countProfileEmbeddings(t, db); got != 0 {
			t.Fatalf("total orphan profile_embedding rows = %d, want 0", got)
		}
	})

	t.Run("operations set and accept run embedding outside SQLite write tx and never roll back on embed failure", func(t *testing.T) {
		db := operationtest.OpenDatabase(t, Schema())
		embedInTx := false
		embedCalls := 0
		embedder := &profileTestEmbedder{
			vectors: map[string][]float32{
				"editor: neovim":    {1, 0, 0, 0},
				"language: golang":  {0, 1, 0, 0},
				"terminal: ghostty": {0, 0, 1, 0},
				"browser: zen":      {0, 0, 0, 1},
			},
			onEmbed: func(ctx stdcontext.Context, _ embedding.Role, _ []string) {
				embedCalls++
				if sqlite.InTx(ctx, db) {
					embedInTx = true
				}
			},
		}
		cfg := Default()
		cfg.EmbeddingEnabled = true
		mod, err := New(t.Context(), &Deps{
			Config:   cfg,
			DB:       db,
			Scanner:  secret.New(),
			Embedder: embedder,
			Embedding: &EmbeddingSettings{
				Fingerprint: "fp-ops",
				Dimensions:  4,
			},
		})
		if err != nil {
			t.Fatalf("profile.New: %v", err)
		}
		rig := operationtest.NewRig(t, &operationtest.Options{
			DB:       db,
			Catalogs: []operation.Catalog{Catalog()},
			Bindings: func(*operationtest.Stubs) []operation.Binding { return mod.Bindings() },
		})

		_, err = rig.Registry.Invoke(t.Context(), "profile.set", &operation.Call{
			Caller:         operation.Caller{Surface: operation.SurfaceCLI, PrincipalID: principal.OwnerID, Class: principal.ClassOwner},
			Input:          json.RawMessage(`{"category":"tool","key":"editor","value":"neovim"}`),
			IdempotencyKey: "k-set-1",
		})
		if err != nil {
			t.Fatalf("profile.set: %v", err)
		}
		setFactID := FactID(principal.OwnerID, "tool", "editor", ValueDigest("neovim"))
		if embedInTx {
			t.Fatal("profile.set invoked Embedder inside an active SQLite transaction")
		}
		if embedCalls != 1 || !hasProfileEmbedding(t, db, setFactID) {
			t.Fatalf("profile.set post-commit embed: calls=%d embedded=%t, want 1/true", embedCalls, hasProfileEmbedding(t, db, setFactID))
		}

		rawStore, err := NewSQLiteStore(db)
		if err != nil {
			t.Fatalf("NewSQLiteStore: %v", err)
		}
		candID := FactID(principal.OwnerID, "language", "language", ValueDigest("golang"))
		if err := rawStore.InsertFact(t.Context(), &Fact{
			ID: candID, OwnerID: principal.OwnerID, Category: "language", Key: "language", Value: "golang",
			ValueDigest: ValueDigest("golang"), Status: StatusCandidate, Origin: OriginDerived, OwnerVerified: false,
			Confidence: 0.6, ConfidencePolicy: ConfidencePolicyV1, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("InsertFact(candidate): %v", err)
		}
		_, err = rig.Registry.Invoke(t.Context(), "profile.accept", &operation.Call{
			Caller:         operation.Caller{Surface: operation.SurfaceCLI, PrincipalID: principal.OwnerID, Class: principal.ClassOwner},
			Input:          json.RawMessage(`{"id":"` + candID + `"}`),
			IdempotencyKey: "k-accept-1",
		})
		if err != nil {
			t.Fatalf("profile.accept: %v", err)
		}
		if embedInTx {
			t.Fatal("profile.accept invoked Embedder inside an active SQLite transaction")
		}
		if embedCalls != 2 || !hasProfileEmbedding(t, db, candID) {
			t.Fatalf("profile.accept post-commit embed: calls=%d embedded=%t, want 2/true", embedCalls, hasProfileEmbedding(t, db, candID))
		}

		embedder.mu.Lock()
		embedder.err = errors.New("embedding provider outage")
		embedder.mu.Unlock()

		_, err = rig.Registry.Invoke(t.Context(), "profile.set", &operation.Call{
			Caller:         operation.Caller{Surface: operation.SurfaceCLI, PrincipalID: principal.OwnerID, Class: principal.ClassOwner},
			Input:          json.RawMessage(`{"category":"tool","key":"terminal","value":"ghostty"}`),
			IdempotencyKey: "k-set-fail-1",
		})
		if err != nil {
			t.Fatalf("profile.set failed when embedder errored: %v", err)
		}
		failSetID := FactID(principal.OwnerID, "tool", "terminal", ValueDigest("ghostty"))
		persisted, err := mod.Service().GetFact(t.Context(), principal.OwnerID, failSetID)
		if err != nil || persisted.Status != StatusActive || !persisted.OwnerVerified {
			t.Fatalf("profile.set rolled back on embed error: (%+v, %v)", persisted, err)
		}
		if hasProfileEmbedding(t, db, failSetID) {
			t.Fatal("expected no embedding when embedder errored")
		}

		candFailID := FactID(principal.OwnerID, "tool", "browser", ValueDigest("zen"))
		if err := rawStore.InsertFact(t.Context(), &Fact{
			ID: candFailID, OwnerID: principal.OwnerID, Category: "tool", Key: "browser", Value: "zen",
			ValueDigest: ValueDigest("zen"), Status: StatusCandidate, Origin: OriginDerived, OwnerVerified: false,
			Confidence: 0.6, ConfidencePolicy: ConfidencePolicyV1, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("InsertFact(candidate fail): %v", err)
		}
		_, err = rig.Registry.Invoke(t.Context(), "profile.accept", &operation.Call{
			Caller:         operation.Caller{Surface: operation.SurfaceCLI, PrincipalID: principal.OwnerID, Class: principal.ClassOwner},
			Input:          json.RawMessage(`{"id":"` + candFailID + `"}`),
			IdempotencyKey: "k-accept-fail-1",
		})
		if err != nil {
			t.Fatalf("profile.accept failed when embedder errored: %v", err)
		}
		acceptedFail, err := mod.Service().GetFact(t.Context(), principal.OwnerID, candFailID)
		if err != nil || acceptedFail.Status != StatusActive || !acceptedFail.OwnerVerified {
			t.Fatalf("profile.accept rolled back on embed error: (%+v, %v)", acceptedFail, err)
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
		if err := store.upsertFactEmbeddings(t.Context(), "fp-fb", []profileVectorWrite{{
			FactID:        "pf-go",
			FactKey:       "backend",
			FactValue:     "golang systems programming",
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

func TestProfileEmbeddingBackfillSurvivesPermanentlyBlockedNewestRows(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	_, db := newTestSQLiteStore(t)
	embedder := &profileTestEmbedder{
		vectors: map[string][]float32{
			"destination: terbang ke Jepang": {1, 0, 0, 0},
		},
	}
	store, err := NewSQLiteStoreWithEmbedding(db, &EmbeddingOptions{
		Enabled:  true,
		Embedder: embedder,
		Settings: &EmbeddingSettings{Fingerprint: "fp-starve", Dimensions: 4, BatchSize: 50},
	})
	if err != nil {
		t.Fatalf("NewSQLiteStoreWithEmbedding: %v", err)
	}

	awsKey := "AKIA" + "ABCDEFGHIJKLMNOP"
	blocked := maxBackfillRowsPerTick + 20
	for i := range blocked {
		seedActiveProfileFact(t, store, fmt.Sprintf("pf-blocked-%03d", i), "preference",
			fmt.Sprintf("credential_%03d", i), "value "+awsKey, true, now.Add(-time.Duration(blocked-i)*time.Minute))
	}
	seedActiveProfileFact(t, store, "pf-eligible", "preference", "destination", "terbang ke Jepang", true, now.Add(-24*time.Hour))

	settings := &EmbeddingSettings{Fingerprint: "fp-starve", Dimensions: 4}
	healthCheck := &profileEmbeddingHealth{store: store, settings: settings, available: true}

	if embedded, err := store.BackfillEmbeddingsOnce(t.Context(), maxBackfillRowsPerTick); err != nil || embedded != 0 {
		t.Fatalf("first tick = (%d, %v), want (0, nil) while blocked rows fill the scan budget", embedded, err)
	}
	if findings := healthCheck.Check(t.Context()); len(findings) != 1 || findings[0].Code != "coverage_pending" || findings[0].Status != health.StatusDegraded {
		t.Fatalf("health with embedded<total and stale=0 = %+v, want degraded coverage_pending", findings)
	}

	if embedded, err := store.BackfillEmbeddingsOnce(t.Context(), maxBackfillRowsPerTick); err != nil || embedded != 1 {
		t.Fatalf("second tick = (%d, %v), want (1, nil) once the cursor resumes past blocked rows", embedded, err)
	}
	if !hasProfileEmbedding(t, db, "pf-eligible") {
		t.Fatal("older eligible fact never gained an embedding behind the blocked run")
	}
	if embedder.sentContains(awsKey) {
		t.Fatal("credential-bearing blocked fact was sent to the embedder")
	}
	if findings := healthCheck.Check(t.Context()); len(findings) != 1 || findings[0].Code != "coverage_pending" || findings[0].Status != health.StatusDegraded {
		t.Fatalf("health after bounded backfill = %+v, want degraded coverage_pending for the permanent gap", findings)
	}
}

func TestProfileVectorSearchFailureIsRecordedAndFallsBackLexically(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

	t.Run("database scan error logs a warning and keeps lexical results", func(t *testing.T) {
		_, db := newTestSQLiteStore(t)
		records := &recordingLogHandler{}
		embedder := &profileTestEmbedder{vectors: map[string][]float32{"golang systems": {1, 0, 0, 0}}}
		store, err := NewSQLiteStoreWithEmbedding(db, &EmbeddingOptions{
			Enabled:  true,
			Embedder: embedder,
			Settings: &EmbeddingSettings{Fingerprint: "fp-fail", Dimensions: 4},
			Logger:   slog.New(records),
		})
		if err != nil {
			t.Fatalf("NewSQLiteStoreWithEmbedding: %v", err)
		}
		seedActiveProfileFact(t, store, "pf-go", "language", "backend", "golang systems programming", true, now.Add(-time.Hour))

		if _, err := db.ExecContext(t.Context(), `DROP TABLE profile_embedding`); err != nil {
			t.Fatalf("drop profile_embedding: %v", err)
		}

		facts, err := store.SearchFacts(t.Context(), "owner-1", "", "golang systems", true, 5, now)
		if err != nil {
			t.Fatalf("SearchFacts must keep lexical results on a vector scan failure: %v", err)
		}
		if len(facts) != 1 || facts[0].ID != "pf-go" {
			t.Fatalf("SearchFacts = %+v, want lexical hit pf-go", facts)
		}
		warn := records.warnings()
		if len(warn) != 1 {
			t.Fatalf("vector scan failure warnings = %d, want 1", len(warn))
		}
		if warn[0].Message != "profile vector search failed" {
			t.Fatalf("warning message = %q, want profile vector search failed", warn[0].Message)
		}
	})

	t.Run("row scan error is returned instead of silently dropping the vector leg", func(t *testing.T) {
		_, db := newTestSQLiteStore(t)
		store, err := NewSQLiteStoreWithEmbedding(db, &EmbeddingOptions{
			Enabled:  true,
			Embedder: &profileTestEmbedder{vectors: map[string][]float32{"golang systems": {1, 0, 0, 0}}},
			Settings: &EmbeddingSettings{Fingerprint: "fp-scan", Dimensions: 4},
		})
		if err != nil {
			t.Fatalf("NewSQLiteStoreWithEmbedding: %v", err)
		}
		seedActiveProfileFact(t, store, "pf-corrupt", "language", "backend", "golang systems programming", true, now.Add(-time.Hour))
		if err := store.upsertFactEmbeddings(t.Context(), "fp-scan", []profileVectorWrite{{
			FactID:        "pf-corrupt",
			FactKey:       "backend",
			FactValue:     "golang systems programming",
			ContentSHA256: factContentSHA256(factEmbedText("backend", "golang systems programming")),
			Vector:        []float32{1, 0, 0, 0},
			CreatedAt:     now,
		}}); err != nil {
			t.Fatalf("upsertFactEmbeddings: %v", err)
		}
		if _, err := db.ExecContext(t.Context(), `UPDATE profile_fact SET created_at = 'not-a-time' WHERE id = 'pf-corrupt'`); err != nil {
			t.Fatalf("corrupt profile_fact timestamp: %v", err)
		}

		hits, ran, err := store.searchFactVectors(t.Context(), "owner-1", "", "golang systems", 5, now)
		if err == nil {
			t.Fatalf("searchFactVectors = (%+v, ran=%t, nil), want a scan error", hits, ran)
		}
		if ran || hits != nil {
			t.Fatalf("searchFactVectors on scan error = (%+v, ran=%t), want (nil, false)", hits, ran)
		}
	})
}

type recordingLogHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingLogHandler) Enabled(stdcontext.Context, slog.Level) bool { return true }

func (h *recordingLogHandler) Handle(_ stdcontext.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, record.Clone())
	return nil
}

func (h *recordingLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *recordingLogHandler) WithGroup(string) slog.Handler { return h }

func (h *recordingLogHandler) warnings() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]slog.Record, 0, len(h.records))
	for i := range h.records {
		if h.records[i].Level == slog.LevelWarn {
			out = append(out, h.records[i])
		}
	}
	return out
}
