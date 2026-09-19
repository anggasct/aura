package store

import (
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"
)

func testProfileFact(id string) *ProfileFact {
	now := time.Now().UTC().Truncate(time.Second)
	return &ProfileFact{
		ID: id, OwnerID: "owner-1", Category: "language", Key: "backend",
		Value: "Go", ValueDigest: "digest-go", Status: "candidate", Origin: "derived",
		Confidence: 0.5, ConfidencePolicy: "conf-v1", CreatedAt: now, UpdatedAt: now,
	}
}

func seedProfileEvent(t *testing.T, db *sql.DB, sessionID, eventID string) {
	t.Helper()
	seedMemorySession(t, db, sessionID, "owner-1")
	events := NewEventStore(db)
	if _, err := events.AppendSequenced(t.Context(), sessionID, &RuntimeEvent{
		ID: eventID, SessionID: sessionID, TurnID: "t1", InvocationID: "inv-1",
		Author: "owner-1", Kind: "message.completed", SchemaVersion: 1,
		Payload: []byte(`{"text":"i prefer go"}`), CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("AppendSequenced: %v", err)
	}
}

func TestProfileStore_RoundTrip(t *testing.T) {
	db := newTestDB(t)
	s := NewProfileStore(db)
	ctx := t.Context()
	seedProfileEvent(t, db, "sess-1", "evt-1")
	if err := s.InsertFact(ctx, testProfileFact("pf-1")); err != nil {
		t.Fatalf("InsertFact: %v", err)
	}
	got, found, err := s.GetFact(ctx, "pf-1")
	if err != nil || !found {
		t.Fatalf("GetFact: %v, %v", err, found)
	}
	if got.Value != "Go" || got.Confidence != 0.5 || got.OwnerVerified {
		t.Errorf("fact = %+v", got)
	}
	added, err := s.AddEvidence(ctx, &ProfileEvidence{
		FactID: "pf-1", SourceEventID: "evt-1", SourceDigest: "d1",
		Provider: "p", Model: "m", PromptVersion: "v1", ObservedAt: time.Now().UTC(),
	})
	if err != nil || !added {
		t.Fatalf("AddEvidence: %v, %v", err, added)
	}
	added, err = s.AddEvidence(ctx, &ProfileEvidence{
		FactID: "pf-1", SourceEventID: "evt-1", SourceDigest: "d1", ObservedAt: time.Now().UTC(),
	})
	if err != nil || added {
		t.Fatalf("duplicate evidence: %v, %v", err, added)
	}
	evidence, err := s.ListEvidence(ctx, "pf-1")
	if err != nil || len(evidence) != 1 || evidence[0].SourceDigest != "d1" {
		t.Fatalf("ListEvidence: %+v, %v", evidence, err)
	}
	update := testProfileFact("pf-1")
	update.Status = "active"
	update.Confidence = 0.7
	if err := s.UpdateFact(ctx, update); err != nil {
		t.Fatalf("UpdateFact: %v", err)
	}
	active, err := s.ListActive(ctx, "owner-1", "language", "backend")
	if err != nil || len(active) != 1 || active[0].Status != "active" {
		t.Fatalf("ListActive: %+v, %v", active, err)
	}
}

func TestProfileStore_Constraints(t *testing.T) {
	db := newTestDB(t)
	s := NewProfileStore(db)
	ctx := t.Context()
	cases := map[string]func(*ProfileFact){
		"bad category":   func(f *ProfileFact) { f.Category = "mood" },
		"bad status":     func(f *ProfileFact) { f.Status = "archived" },
		"bad origin":     func(f *ProfileFact) { f.Origin = "model" },
		"bad confidence": func(f *ProfileFact) { f.Confidence = 1.5 },
		"empty policy":   func(f *ProfileFact) { f.ConfidencePolicy = "" },
		"empty key":      func(f *ProfileFact) { f.Key = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			fact := testProfileFact("pf-" + strings.ReplaceAll(name, " ", "-"))
			mutate(fact)
			if err := s.InsertFact(ctx, fact); err == nil {
				t.Errorf("expected rejection")
			} else if code, ok := CodeOf(err); !ok || code != ErrorCodeProfileInvalid {
				t.Errorf("code = %v, %v (%v)", code, ok, err)
			}
		})
	}
	if err := s.InsertFact(ctx, testProfileFact("pf-dup")); err != nil {
		t.Fatalf("InsertFact: %v", err)
	}
	if err := s.InsertFact(ctx, testProfileFact("pf-dup")); err == nil {
		t.Errorf("expected duplicate rejection")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeProfileConflict {
		t.Errorf("code = %v, %v (%v)", code, ok, err)
	}
	if err := s.UpdateFact(ctx, testProfileFact("pf-missing")); err == nil {
		t.Errorf("expected not-found")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeProfileNotFound {
		t.Errorf("code = %v, %v (%v)", code, ok, err)
	}
	if _, _, err := s.GetFact(ctx, "pf-missing"); err != nil {
		t.Fatalf("GetFact: %v", err)
	}
	if _, err := s.AddEvidence(ctx, &ProfileEvidence{FactID: "pf-missing", SourceEventID: "evt-missing", SourceDigest: "d", ObservedAt: time.Now().UTC()}); err == nil {
		t.Errorf("expected foreign key rejection")
	}
}

func TestProfileStore_FTSIndex(t *testing.T) {
	db := newTestDB(t)
	s := NewProfileStore(db)
	ctx := t.Context()
	fact := testProfileFact("pf-fts")
	fact.Status = "active"
	if err := s.InsertFact(ctx, fact); err != nil {
		t.Fatalf("InsertFact: %v", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM profile_fact_fts WHERE profile_fact_fts MATCH 'backend'`).Scan(&count); err != nil {
		t.Fatalf("fts query: %v", err)
	}
	if count != 1 {
		t.Errorf("fts matches = %d", count)
	}
	update := testProfileFact("pf-fts")
	update.Status = "active"
	update.Value = "Rust"
	if err := s.UpdateFact(ctx, update); err != nil {
		t.Fatalf("UpdateFact: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM profile_fact_fts WHERE profile_fact_fts MATCH 'Rust'`).Scan(&count); err != nil {
		t.Fatalf("fts query: %v", err)
	}
	if count != 1 {
		t.Errorf("fts matches after update = %d", count)
	}
}

func TestProfileStore_MigrationTables(t *testing.T) {
	db := newTestDB(t)
	for _, table := range []string{"profile_fact", "profile_evidence", "profile_fact_fts"} {
		var name string
		if err := db.QueryRowContext(t.Context(), `SELECT name FROM sqlite_master WHERE name = ?`, table).Scan(&name); err != nil {
			t.Fatalf("table %s: %v", table, err)
		}
	}
}

func TestProfileStore_ConcurrentInsertConverges(t *testing.T) {
	db := newTestDB(t)
	s := NewProfileStore(db)
	ctx := t.Context()
	seedProfileEvent(t, db, "sess-1", "evt-1")
	const workers = 8
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fact := testProfileFact("pf-race")
			_ = s.InsertFact(ctx, fact)
			_, _ = s.AddEvidence(ctx, &ProfileEvidence{
				FactID: "pf-race", SourceEventID: "evt-1", SourceDigest: "shared",
				ObservedAt: time.Now().UTC(),
			})
		}()
	}
	wg.Wait()
	got, found, err := s.GetFact(ctx, "pf-race")
	if err != nil || !found {
		t.Fatalf("GetFact: %v, %v", err, found)
	}
	if got.Value != "Go" {
		t.Errorf("fact = %+v", got)
	}
	evidence, err := s.ListEvidence(ctx, "pf-race")
	if err != nil || len(evidence) != 1 {
		t.Errorf("evidence = %+v, %v", evidence, err)
	}
}

func TestProfileStore_SearchUsesCallerClock(t *testing.T) {
	db := newTestDB(t)
	s := NewProfileStore(db)
	ctx := t.Context()
	wall := time.Now().UTC().Truncate(time.Second)
	expiry := wall.Add(time.Hour)
	futureFact := testProfileFact("pf-future")
	futureFact.Status = "active"
	futureFact.Value = "wall-future-value"
	futureFact.ExpiresAt = &expiry
	if err := s.InsertFact(ctx, futureFact); err != nil {
		t.Fatalf("InsertFact future: %v", err)
	}
	pastExpiry := wall.Add(-time.Hour)
	pastFact := testProfileFact("pf-past")
	pastFact.Status = "active"
	pastFact.Value = "wall-past-value"
	pastFact.Key = "backend-past"
	pastFact.ExpiresAt = &pastExpiry
	if err := s.InsertFact(ctx, pastFact); err != nil {
		t.Fatalf("InsertFact past: %v", err)
	}
	callerBeforeExpiry := expiry.Add(-time.Minute)
	for _, query := range []string{"", "wall-future-value"} {
		hits, err := s.SearchFacts(ctx, "owner-1", "", query, 10, callerBeforeExpiry)
		if err != nil {
			t.Fatalf("SearchFacts(%q) before expiry: %v", query, err)
		}
		found := false
		for _, hit := range hits {
			if hit.ID == "pf-future" {
				found = true
			}
		}
		if query == "" && !found {
			t.Errorf("future fact missing before caller expiry (query %q)", query)
		}
	}
	callerAfterExpiry := expiry.Add(time.Minute)
	for _, query := range []string{"", "wall-future-value"} {
		hits, err := s.SearchFacts(ctx, "owner-1", "", query, 10, callerAfterExpiry)
		if err != nil {
			t.Fatalf("SearchFacts(%q) after expiry: %v", query, err)
		}
		for _, hit := range hits {
			if hit.ID == "pf-future" {
				t.Errorf("expired fact returned with caller clock (query %q)", query)
			}
		}
	}
	callerBeforePastExpiry := pastExpiry.Add(-time.Minute)
	hits, err := s.SearchFacts(ctx, "owner-1", "", "", 10, callerBeforePastExpiry)
	if err != nil {
		t.Fatalf("SearchFacts before past expiry: %v", err)
	}
	found := false
	for _, hit := range hits {
		if hit.ID == "pf-past" {
			found = true
		}
	}
	if !found {
		t.Errorf("fact valid per caller clock missing; wall clock must not decide expiry")
	}
}
