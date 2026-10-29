package store

import (
	"testing"
	"time"
)

func TestRekeyOwners_DryRunApplyIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	osUser := OwnerMove{From: "kuma", To: "local-owner"}
	trusted := OwnerMove{From: "111", To: "discord:111"}
	seedMemorySession(t, db, "terminal-1", "kuma")
	seedMemorySession(t, db, "dm:111", "111")
	seedMemorySession(t, db, "dm:777", "local-owner")
	memories := NewMemoryStore(db)
	ownerDoc := testMemoryDocument("mem-owner", "terminal-1", MemoryKindEventText)
	ownerDoc.OwnerID = "kuma"
	trustedDoc := testMemoryDocument("mem-trusted", "dm:111", MemoryKindEventText)
	trustedDoc.OwnerID = "111"
	trustedDoc.Content = "the weekly report shipped"
	for _, doc := range []*MemoryDocument{ownerDoc, trustedDoc} {
		if _, err := memories.UpsertDocument(ctx, doc); err != nil {
			t.Fatalf("UpsertDocument(%s): %v", doc.ID, err)
		}
	}
	profiles := NewProfileStore(db)
	moving := testProfileFact("pf-moving")
	moving.OwnerID = "kuma"
	moving.Status = "active"
	colliding := testProfileFact("pf-colliding")
	colliding.OwnerID = "kuma"
	colliding.Key, colliding.ValueDigest = "editor", "digest-nvim"
	existing := testProfileFact("pf-existing")
	existing.OwnerID = "local-owner"
	existing.Key, existing.ValueDigest = "editor", "digest-nvim"
	for _, fact := range []*ProfileFact{moving, colliding, existing} {
		if err := profiles.InsertFact(ctx, fact); err != nil {
			t.Fatalf("InsertFact(%s): %v", fact.ID, err)
		}
	}
	want := OwnerRekeyReport{Sessions: 2, MemoryDocuments: 2, ProfileFacts: 1, ProfileFactConflicts: 1}

	preview, err := RekeyOwners(ctx, db, []OwnerMove{osUser, trusted}, false)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if preview != want {
		t.Fatalf("dry run = %+v, want %+v", preview, want)
	}
	var untouched int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session WHERE owner_id IN ('kuma','111')`).Scan(&untouched); err != nil || untouched != 2 {
		t.Fatalf("dry run wrote: %d legacy sessions left, %v", untouched, err)
	}

	applied, err := RekeyOwners(ctx, db, []OwnerMove{osUser, trusted}, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if applied != want {
		t.Fatalf("apply = %+v, want %+v", applied, want)
	}
	hits, err := memories.Search(ctx, &MemoryQuery{OwnerID: "local-owner", Terms: "nightly backup", Limit: 10})
	if err != nil || len(hits) != 1 || hits[0].ID != "mem-owner" {
		t.Fatalf("owner search after rekey = %+v, %v", hits, err)
	}
	hits, err = memories.Search(ctx, &MemoryQuery{OwnerID: "discord:111", Terms: "weekly report", Limit: 10})
	if err != nil || len(hits) != 1 {
		t.Fatalf("trusted search after rekey = %+v, %v", hits, err)
	}
	facts, err := profiles.SearchFacts(ctx, "local-owner", "", "backend", false, 10, time.Now().UTC())
	if err != nil || len(facts) != 1 || facts[0].ID != "pf-moving" {
		t.Fatalf("profile search after rekey = %+v, %v", facts, err)
	}
	if kept, found, err := profiles.GetFact(ctx, "pf-colliding"); err != nil || !found || kept.OwnerID != "kuma" {
		t.Fatalf("colliding fact = %+v, %v, %v; want left under the source", kept, found, err)
	}

	again, err := RekeyOwners(ctx, db, []OwnerMove{osUser, trusted}, true)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if again.Moved() != 0 || again.ProfileFactConflicts != 1 {
		t.Fatalf("second apply = %+v, want zero moves and the standing conflict", again)
	}
}

func TestRekeyOwners_NoMatchesReportsZero(t *testing.T) {
	db := newTestDB(t)
	report, err := RekeyOwners(t.Context(), db, []OwnerMove{{From: "kuma", To: "local-owner"}}, true)
	if err != nil || report != (OwnerRekeyReport{}) {
		t.Fatalf("report = %+v, %v; want zeros", report, err)
	}
}

func TestRekeyOwners_RejectsInvalidMoves(t *testing.T) {
	db := newTestDB(t)
	for name, moves := range map[string][]OwnerMove{
		"empty source":     {{From: "", To: "local-owner"}},
		"empty target":     {{From: "kuma", To: " "}},
		"self move":        {{From: "local-owner", To: "local-owner"}},
		"duplicate source": {{From: "kuma", To: "local-owner"}, {From: "kuma", To: "discord:1"}},
		"chained move":     {{From: "kuma", To: "local-owner"}, {From: "local-owner", To: "x"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := RekeyOwners(t.Context(), db, moves, false)
			if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
				t.Fatalf("code = %q, %v; want invalid_argument", code, ok)
			}
		})
	}
}
