package memory

import (
	"fmt"
	"math"
	"testing"
	"time"
)

func newSQLiteMemoryService(t *testing.T) (*Service, *SQLiteStore) {
	t.Helper()
	db, store := newTestSQLiteStore(t)
	seedMemorySession(t, db, "sess-1", "owner-1")
	service, err := NewService(store, nil, &ServiceOptions{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return service, store
}

func seedScoredDoc(t *testing.T, store *SQLiteStore, id, content string, seq int64, createdAt time.Time) {
	t.Helper()
	clean := false
	doc := &StoredDocument{
		ID:           id,
		OwnerID:      "owner-1",
		SessionID:    "sess-1",
		Kind:         KindEventText,
		FromSequence: seq,
		ToSequence:   seq,
		Content:      content,
		TrustLabel:   "owner_input",
		Tainted:      &clean,
		CreatedAt:    createdAt,
	}
	if _, err := store.UpsertDocument(t.Context(), doc); err != nil {
		t.Fatalf("UpsertDocument(%s): %v", id, err)
	}
}

func TestMemoryRecallFallsBackToOROnNaturalQueries(t *testing.T) {
	service, store := newSQLiteMemoryService(t)
	ctx := t.Context()
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

	seedScoredDoc(t, store, "mem-id-plan", "rencana liburan Jepang bulan depan", 1, now.Add(-120*24*time.Hour))
	seedScoredDoc(t, store, "mem-id-partial", "liburan singkat akhir pekan", 2, now.Add(-24*time.Hour))
	seedScoredDoc(t, store, "mem-en-plan", "postgres migration schedule finalized", 3, now.Add(-10*24*time.Hour))
	seedScoredDoc(t, store, "mem-unrelated", "catatan belanja sayur mingguan", 4, now.Add(-time.Hour))

	idHits, err := service.Recall(ctx, &RecallRequest{
		OwnerID:   "owner-1",
		AgentIDs:  mainPersona,
		SessionID: "sess-1",
		Query:     "eh minggu lalu kita bahas rencana liburan ke Jepang kan?",
		Now:       now,
	})
	if err != nil {
		t.Fatalf("Recall(id natural): %v", err)
	}
	if len(idHits) == 0 {
		t.Fatalf("Recall(id natural) returned 0 hits, want fallback OR match")
	}
	if idHits[0].ID != "mem-id-plan" {
		t.Errorf("top Indonesian hit = %s, want mem-id-plan (hits=%+v)", idHits[0].ID, idHits)
	}
	if math.IsNaN(idHits[0].Score) || idHits[0].Score <= 0 {
		t.Errorf("top Indonesian hit score = %v, want positive finite score", idHits[0].Score)
	}

	enHits, err := service.Recall(ctx, &RecallRequest{
		OwnerID:   "owner-1",
		AgentIDs:  mainPersona,
		SessionID: "sess-1",
		Query:     "hey did we discuss the postgres migration schedule yesterday?",
		Now:       now,
	})
	if err != nil {
		t.Fatalf("Recall(en natural): %v", err)
	}
	if len(enHits) == 0 || enHits[0].ID != "mem-en-plan" {
		t.Fatalf("Recall(en natural) = %+v, want mem-en-plan first", enHits)
	}

	tiered, err := service.Recall(ctx, &RecallRequest{
		OwnerID:   "owner-1",
		AgentIDs:  mainPersona,
		SessionID: "sess-1",
		Query:     "rencana liburan",
		Now:       now,
	})
	if err != nil {
		t.Fatalf("Recall(tiered): %v", err)
	}
	if len(tiered) < 2 {
		t.Fatalf("tiered hits = %d, want at least 2", len(tiered))
	}
	if tiered[0].ID != "mem-id-plan" || tiered[1].ID != "mem-id-partial" {
		t.Errorf("tiered order = [%s, %s], want Stage 1 exact match mem-id-plan ahead of newer Stage 2 partial match mem-id-partial", tiered[0].ID, tiered[1].ID)
	}
}

func TestMemoryForgetByQueryStaysStrictAND(t *testing.T) {
	service, store := newSQLiteMemoryService(t)
	ctx := t.Context()
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

	seedScoredDoc(t, store, "mem-exact", "rencana liburan Jepang bulan depan", 1, now.Add(-24*time.Hour))
	seedScoredDoc(t, store, "mem-partial", "liburan singkat akhir pekan", 2, now.Add(-time.Hour))

	preview, err := service.PreviewForgetByQuery(ctx, &DocumentQuery{
		OwnerID: "owner-1",
		Terms:   "rencana liburan",
		Limit:   10,
	})
	if err != nil {
		t.Fatalf("PreviewForgetByQuery(exact+partial): %v", err)
	}
	if len(preview) != 1 || preview[0].ID != "mem-exact" {
		t.Fatalf("PreviewForgetByQuery = %+v, want only strict AND match mem-exact", preview)
	}

	naturalPreview, err := service.PreviewForgetByQuery(ctx, &DocumentQuery{
		OwnerID: "owner-1",
		Terms:   "eh minggu lalu kita bahas rencana liburan ke Jepang kan?",
		Limit:   10,
	})
	if err != nil {
		t.Fatalf("PreviewForgetByQuery(natural): %v", err)
	}
	if len(naturalPreview) != 0 {
		t.Fatalf("PreviewForgetByQuery(natural) = %+v, want 0 (must not broaden to OR)", naturalPreview)
	}

	deleted, err := service.Forget(ctx, "owner-1", preview)
	if err != nil || deleted != 1 {
		t.Fatalf("Forget = %d, %v; want 1", deleted, err)
	}
	if _, found, err := store.GetDocument(ctx, "mem-partial"); err != nil || !found {
		t.Fatalf("partial match doc deleted: found=%v err=%v", found, err)
	}
}

func TestMemoryRecallPrefersRecentDocumentsAtEqualRelevance(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

	t.Run("equal bm25 prefers recent document", func(t *testing.T) {
		service, store := newSQLiteMemoryService(t)
		ctx := t.Context()

		seedScoredDoc(t, store, "mem-a-old", "deployment checklist alpha", 1, now.Add(-120*24*time.Hour))
		seedScoredDoc(t, store, "mem-z-recent", "deployment checklist bravo", 2, now.Add(-24*time.Hour))

		docs, err := service.Recall(ctx, &RecallRequest{
			OwnerID:   "owner-1",
			AgentIDs:  mainPersona,
			SessionID: "sess-1",
			Query:     "deployment checklist",
			Now:       now,
		})
		if err != nil {
			t.Fatalf("Recall: %v", err)
		}
		if len(docs) != 2 {
			t.Fatalf("len(docs) = %d, want 2", len(docs))
		}
		if docs[0].ID != "mem-z-recent" || docs[1].ID != "mem-a-old" {
			t.Fatalf("order = [%s (score=%v), %s (score=%v)], want mem-z-recent before mem-a-old", docs[0].ID, docs[0].Score, docs[1].ID, docs[1].Score)
		}
		if !(docs[0].Score > docs[1].Score) {
			t.Errorf("recent score %v not strictly greater than older score %v", docs[0].Score, docs[1].Score)
		}
	})

	t.Run("single hit scores cleanly without NaN", func(t *testing.T) {
		service, store := newSQLiteMemoryService(t)
		ctx := t.Context()

		seedScoredDoc(t, store, "mem-only", "unique telemetry blueprint", 1, now.Add(-5*24*time.Hour))

		docs, err := service.Recall(ctx, &RecallRequest{
			OwnerID:   "owner-1",
			AgentIDs:  mainPersona,
			SessionID: "sess-1",
			Query:     "telemetry",
			Now:       now,
		})
		if err != nil {
			t.Fatalf("Recall: %v", err)
		}
		if len(docs) != 1 || docs[0].ID != "mem-only" {
			t.Fatalf("docs = %+v, want single hit mem-only", docs)
		}
		if math.IsNaN(docs[0].Score) || math.IsInf(docs[0].Score, 0) || docs[0].Score <= 0 || docs[0].Score > 1 {
			t.Errorf("single-hit score = %v, want finite value in (0, 1]", docs[0].Score)
		}
	})

	t.Run("strong older match still outranks weak recent hit", func(t *testing.T) {
		service, store := newSQLiteMemoryService(t)
		ctx := t.Context()

		for i := range 20 {
			seedScoredDoc(t, store, fmt.Sprintf("mem-bg-%02d", i), fmt.Sprintf("general routine journal entry number %d about daily errands", i), int64(i+1), now.Add(-10*24*time.Hour))
		}
		seedScoredDoc(t, store, "mem-strong-old", "kubernetes failover runbook kubernetes failover runbook", 30, now.Add(-120*24*time.Hour))
		seedScoredDoc(t, store, "mem-weak-recent", "long meeting recap covering kitchen renovation groceries laundry weekend travel traffic weather cinema books music sports gardening painting and briefly mentioning kubernetes once", 31, now.Add(-24*time.Hour))

		rawHits, err := store.Search(ctx, &StoredQuery{
			OwnerID:   "owner-1",
			AgentIDs:  mainPersona,
			SessionID: "sess-1",
			Terms:     "kubernetes",
			Mode:      MatchFallbackOR,
			Now:       now,
			Limit:     10,
		})
		if err != nil {
			t.Fatalf("store.Search: %v", err)
		}
		if len(rawHits) != 2 {
			t.Fatalf("len(rawHits) = %d, want 2", len(rawHits))
		}
		if rawHits[0].ID != "mem-strong-old" || rawHits[1].ID != "mem-weak-recent" {
			t.Fatalf("order = [%s (rank=%v score=%v), %s (rank=%v score=%v)], want strong older doc ahead of weak recent doc",
				rawHits[0].ID, rawHits[0].Rank, rawHits[0].Score, rawHits[1].ID, rawHits[1].Rank, rawHits[1].Score)
		}
		if !(rawHits[0].Rank < rawHits[1].Rank) {
			t.Errorf("expected stronger BM25 contrast: strong=%v weak=%v", rawHits[0].Rank, rawHits[1].Rank)
		}
		recalled, err := service.Recall(ctx, &RecallRequest{
			OwnerID:   "owner-1",
			AgentIDs:  mainPersona,
			SessionID: "sess-1",
			Query:     "kubernetes",
			Now:       now,
		})
		if err != nil {
			t.Fatalf("Recall: %v", err)
		}
		if len(recalled) != 2 || recalled[0].ID != "mem-strong-old" {
			t.Fatalf("recalled = %+v, want strong older doc first", recalled)
		}
	})
}

func TestMemorySearchDoesNotStarveValidHitsBehindSameSecondOrExpiredRows(t *testing.T) {
	_, store := newSQLiteMemoryService(t)
	ctx := t.Context()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	afterWindow := now.Add(500 * time.Millisecond)
	justExpired := now.Add(-500 * time.Millisecond)

	for i := range 60 {
		seedScoredDoc(t, store, fmt.Sprintf("mem-after-%02d", i), "release checklist alpha", int64(i+1), afterWindow)
	}
	seedScoredDoc(t, store, "mem-valid-and", "release checklist alpha", 100, now)
	seedScoredDoc(t, store, "mem-partial-or", "release notes only", 101, now)

	fallbackHits, err := store.Search(ctx, &StoredQuery{
		OwnerID: "owner-1",
		Terms:   "release checklist",
		Mode:    MatchFallbackOR,
		Before:  now,
		Now:     now,
		Limit:   1,
	})
	if err != nil {
		t.Fatalf("Search(MatchFallbackOR): %v", err)
	}
	if len(fallbackHits) != 1 || fallbackHits[0].ID != "mem-valid-and" {
		t.Fatalf("MatchFallbackOR hits = %+v, want mem-valid-and without starvation or erroneous OR fallback", fallbackHits)
	}

	for i := range 60 {
		clean := false
		exp := justExpired
		doc := &StoredDocument{
			ID:           fmt.Sprintf("mem-exp-%02d", i),
			OwnerID:      "owner-1",
			SessionID:    "sess-1",
			Kind:         KindSummary,
			FromSequence: int64(200 + i),
			ToSequence:   int64(200 + i),
			Content:      "strictexpire target alpha",
			TrustLabel:   "owner_input",
			Tainted:      &clean,
			CreatedAt:    now.Add(-time.Hour),
			ExpiresAt:    &exp,
		}
		if _, err := store.UpsertDocument(ctx, doc); err != nil {
			t.Fatalf("UpsertDocument(%s): %v", doc.ID, err)
		}
	}
	seedScoredDoc(t, store, "mem-valid-strict", "strictexpire target alpha", 300, now)

	strictHits, err := store.Search(ctx, &StoredQuery{
		OwnerID: "owner-1",
		Terms:   "strictexpire target",
		Mode:    MatchStrict,
		Now:     now,
		Limit:   1,
	})
	if err != nil {
		t.Fatalf("Search(MatchStrict): %v", err)
	}
	if len(strictHits) != 1 || strictHits[0].ID != "mem-valid-strict" {
		t.Fatalf("MatchStrict hits = %+v, want mem-valid-strict past 60 just-expired rows", strictHits)
	}
}
