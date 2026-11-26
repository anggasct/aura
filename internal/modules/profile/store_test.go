package profile

import (
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

func testStoredFact(id string) *Fact {
	now := time.Now().UTC().Truncate(time.Second)
	return &Fact{
		ID: id, OwnerID: "owner-1", Category: "language", Key: "backend",
		Value: "Go", ValueDigest: "digest-go", Status: StatusCandidate, Origin: OriginDerived,
		Confidence: 0.5, ConfidencePolicy: ConfidencePolicyV1, CreatedAt: now, UpdatedAt: now,
	}
}

func newTestSQLiteStore(t *testing.T) (*SQLiteStore, *sql.DB) {
	t.Helper()
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), Schema())
	s, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	return s, db
}

func seedProfileEvent(t *testing.T, db *sql.DB, sessionID, eventID string) {
	t.Helper()
	now := time.Now().UTC()
	sessions := turnstore.NewSessionService(db)
	if err := sessions.Create(t.Context(), &turn.Session{
		ID: sessionID, OwnerID: "owner-1", CreatedAt: now, UpdatedAt: now, Metadata: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	events := turnstore.NewEventStore(db)
	if _, err := events.AppendSequenced(t.Context(), sessionID, &turn.Event{
		ID: eventID, SessionID: sessionID, TurnID: "t1", InvocationID: "inv-1",
		Author: "owner-1", Kind: "message.completed", SchemaVersion: 1,
		Payload: []byte(`{"text":"i prefer go"}`), CreatedAt: now,
	}); err != nil {
		t.Fatalf("AppendSequenced: %v", err)
	}
}

func TestProfileStore_RoundTrip(t *testing.T) {
	s, db := newTestSQLiteStore(t)
	ctx := t.Context()
	seedProfileEvent(t, db, "sess-1", "evt-1")
	if err := s.InsertFact(ctx, testStoredFact("pf-1")); err != nil {
		t.Fatalf("InsertFact: %v", err)
	}
	got, found, err := s.GetFact(ctx, "pf-1")
	if err != nil || !found {
		t.Fatalf("GetFact: %v, %v", err, found)
	}
	if got.Value != "Go" || got.Confidence != 0.5 || got.OwnerVerified {
		t.Errorf("fact = %+v", got)
	}
	added, err := s.AddEvidence(ctx, "pf-1", &Evidence{
		SourceEventID: "evt-1", SourceDigest: "d1",
		Provider: "p", Model: "m", PromptVersion: "v1", ObservedAt: time.Now().UTC(),
	})
	if err != nil || !added {
		t.Fatalf("AddEvidence: %v, %v", err, added)
	}
	added, err = s.AddEvidence(ctx, "pf-1", &Evidence{
		SourceEventID: "evt-1", SourceDigest: "d1", ObservedAt: time.Now().UTC(),
	})
	if err != nil || added {
		t.Fatalf("duplicate evidence: %v, %v", err, added)
	}
	evidence, err := s.ListEvidence(ctx, "pf-1")
	if err != nil || len(evidence) != 1 || evidence[0].SourceDigest != "d1" {
		t.Fatalf("ListEvidence: %+v, %v", evidence, err)
	}
	update := testStoredFact("pf-1")
	update.Status = StatusActive
	update.Confidence = 0.7
	if err := s.UpdateFact(ctx, update); err != nil {
		t.Fatalf("UpdateFact: %v", err)
	}
	active, err := s.ListActive(ctx, "owner-1", "language", "backend")
	if err != nil || len(active) != 1 || active[0].Status != StatusActive {
		t.Fatalf("ListActive: %+v, %v", active, err)
	}
}

func TestProfileStore_Constraints(t *testing.T) {
	s, _ := newTestSQLiteStore(t)
	ctx := t.Context()
	cases := map[string]func(*Fact){
		"bad category":   func(f *Fact) { f.Category = "mood" },
		"bad status":     func(f *Fact) { f.Status = "archived" },
		"bad origin":     func(f *Fact) { f.Origin = "model" },
		"bad confidence": func(f *Fact) { f.Confidence = 1.5 },
		"empty policy":   func(f *Fact) { f.ConfidencePolicy = "" },
		"empty key":      func(f *Fact) { f.Key = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			fact := testStoredFact("pf-" + strings.ReplaceAll(name, " ", "-"))
			mutate(fact)
			if err := s.InsertFact(ctx, fact); err == nil {
				t.Errorf("expected rejection")
			} else if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeProfileInvalid {
				t.Errorf("code = %v, %v (%v)", code, ok, err)
			}
		})
	}
	if err := s.InsertFact(ctx, testStoredFact("pf-dup")); err != nil {
		t.Fatalf("InsertFact: %v", err)
	}
	if err := s.InsertFact(ctx, testStoredFact("pf-dup")); err == nil {
		t.Errorf("expected duplicate rejection")
	} else if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeProfileConflict {
		t.Errorf("code = %v, %v (%v)", code, ok, err)
	}
	if err := s.UpdateFact(ctx, testStoredFact("pf-missing")); err == nil {
		t.Errorf("expected not-found")
	} else if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeProfileNotFound {
		t.Errorf("code = %v, %v (%v)", code, ok, err)
	}
	if _, _, err := s.GetFact(ctx, "pf-missing"); err != nil {
		t.Fatalf("GetFact: %v", err)
	}
	if _, err := s.AddEvidence(ctx, "pf-missing", &Evidence{SourceEventID: "evt-missing", SourceDigest: "d", ObservedAt: time.Now().UTC()}); err == nil {
		t.Errorf("expected foreign key rejection")
	}
}

func TestProfileStore_FTSIndex(t *testing.T) {
	s, db := newTestSQLiteStore(t)
	ctx := t.Context()
	fact := testStoredFact("pf-fts")
	fact.Status = StatusActive
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
	update := testStoredFact("pf-fts")
	update.Status = StatusActive
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
	_, db := newTestSQLiteStore(t)
	for _, table := range []string{"profile_fact", "profile_evidence", "profile_fact_fts"} {
		var name string
		if err := db.QueryRowContext(t.Context(), `SELECT name FROM sqlite_master WHERE name = ?`, table).Scan(&name); err != nil {
			t.Fatalf("table %s: %v", table, err)
		}
	}
}

func TestProfileStore_ConcurrentInsertConverges(t *testing.T) {
	s, db := newTestSQLiteStore(t)
	ctx := t.Context()
	seedProfileEvent(t, db, "sess-1", "evt-1")
	const workers = 8
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fact := testStoredFact("pf-race")
			_ = s.InsertFact(ctx, fact)
			_, _ = s.AddEvidence(ctx, "pf-race", &Evidence{
				SourceEventID: "evt-1", SourceDigest: "shared",
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
	s, _ := newTestSQLiteStore(t)
	ctx := t.Context()
	wall := time.Now().UTC().Truncate(time.Second)
	expiry := wall.Add(time.Hour)
	futureFact := testStoredFact("pf-future")
	futureFact.Status = StatusActive
	futureFact.Value = "wall-future-value"
	futureFact.ExpiresAt = &expiry
	if err := s.InsertFact(ctx, futureFact); err != nil {
		t.Fatalf("InsertFact future: %v", err)
	}
	pastExpiry := wall.Add(-time.Hour)
	pastFact := testStoredFact("pf-past")
	pastFact.Status = StatusActive
	pastFact.Value = "wall-past-value"
	pastFact.Key = "backend-past"
	pastFact.ExpiresAt = &pastExpiry
	if err := s.InsertFact(ctx, pastFact); err != nil {
		t.Fatalf("InsertFact past: %v", err)
	}
	callerBeforeExpiry := expiry.Add(-time.Minute)
	for _, query := range []string{"", "wall-future-value"} {
		hits, err := s.SearchFacts(ctx, "owner-1", "", query, false, 10, callerBeforeExpiry)
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
		hits, err := s.SearchFacts(ctx, "owner-1", "", query, false, 10, callerAfterExpiry)
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
	hits, err := s.SearchFacts(ctx, "owner-1", "", "", false, 10, callerBeforePastExpiry)
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

func TestProfileStore_SearchVerifiedOnly(t *testing.T) {
	s, _ := newTestSQLiteStore(t)
	ctx := t.Context()
	derived := testStoredFact("pf-derived")
	derived.Status = StatusActive
	derived.Confidence = 0.95
	if err := s.InsertFact(ctx, derived); err != nil {
		t.Fatalf("InsertFact derived: %v", err)
	}
	verified := testStoredFact("pf-verified")
	verified.Status = StatusActive
	verified.Key = "frontend"
	verified.Value = "Go templates"
	verified.ValueDigest = "digest-templates"
	verified.OwnerVerified = true
	if err := s.InsertFact(ctx, verified); err != nil {
		t.Fatalf("InsertFact verified: %v", err)
	}
	now := time.Now().UTC()
	for _, query := range []string{"", "go"} {
		hits, err := s.SearchFacts(ctx, "owner-1", "", query, true, 1, now)
		if err != nil {
			t.Fatalf("SearchFacts(%q, verified): %v", query, err)
		}
		if len(hits) != 1 || hits[0].ID != "pf-verified" {
			t.Errorf("SearchFacts(%q, verified) = %+v, want only the verified fact despite lower confidence", query, hits)
		}
		all, err := s.SearchFacts(ctx, "owner-1", "", query, false, 10, now)
		if err != nil || len(all) != 2 {
			t.Errorf("SearchFacts(%q, all) = %d, %v; want both", query, len(all), err)
		}
	}
}
