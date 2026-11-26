package app

import (
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/profile"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/store"
)

func requireRekeyCounts(t *testing.T, got principal.RekeyReport, sessions, memoryDocs, profileFacts, conflicts int64) {
	t.Helper()
	if got.Count(principal.RekeyLineSessions) != sessions ||
		got.Count(principal.RekeyLineMemoryDocuments) != memoryDocs ||
		got.Count(principal.RekeyLineProfileFacts) != profileFacts ||
		got.Count(principal.RekeyLineProfileFactConflicts) != conflicts {
		t.Fatalf("rekey report = (sessions=%d, memory=%d, facts=%d, conflicts=%d), want (%d, %d, %d, %d)",
			got.Count(principal.RekeyLineSessions),
			got.Count(principal.RekeyLineMemoryDocuments),
			got.Count(principal.RekeyLineProfileFacts),
			got.Count(principal.RekeyLineProfileFactConflicts),
			sessions, memoryDocs, profileFacts, conflicts,
		)
	}
}

func TestRekeyOwners_DryRunApplyIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	osUser := principal.OwnerMove{From: "kuma", To: "local-owner"}
	trusted := principal.OwnerMove{From: "111", To: "discord:111"}
	erasureSeedMemorySession(t, db, "terminal-1", "kuma")
	erasureSeedMemorySession(t, db, "dm:111", "111")
	erasureSeedMemorySession(t, db, "dm:777", "local-owner")
	memories := store.NewMemoryStore(db)
	ownerDoc := testMemoryDocument("mem-owner", "terminal-1", store.MemoryKindEventText)
	ownerDoc.OwnerID = "kuma"
	trustedDoc := testMemoryDocument("mem-trusted", "dm:111", store.MemoryKindEventText)
	trustedDoc.OwnerID = "111"
	trustedDoc.Content = "the weekly report shipped"
	for _, doc := range []*store.MemoryDocument{ownerDoc, trustedDoc} {
		if _, err := memories.UpsertDocument(ctx, doc); err != nil {
			t.Fatalf("UpsertDocument(%s): %v", doc.ID, err)
		}
	}
	profiles, err := profile.NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	moving := testProfileFact("pf-moving")
	moving.OwnerID = "kuma"
	moving.Status = "active"
	colliding := testProfileFact("pf-colliding")
	colliding.OwnerID = "kuma"
	colliding.Key, colliding.ValueDigest = "editor", "digest-nvim"
	existing := testProfileFact("pf-existing")
	existing.OwnerID = "local-owner"
	existing.Key, existing.ValueDigest = "editor", "digest-nvim"
	for _, fact := range []*profile.Fact{moving, colliding, existing} {
		if err := profiles.InsertFact(ctx, fact); err != nil {
			t.Fatalf("InsertFact(%s): %v", fact.ID, err)
		}
	}

	participants, err := rekeyParticipants(db)
	if err != nil {
		t.Fatalf("rekeyParticipants: %v", err)
	}
	preview, err := principal.Rekey(ctx, db, []principal.OwnerMove{osUser, trusted}, false, participants)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	requireRekeyCounts(t, preview, 2, 2, 1, 1)
	var untouched int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session WHERE owner_id IN ('kuma','111')`).Scan(&untouched); err != nil || untouched != 2 {
		t.Fatalf("dry run wrote: %d legacy sessions left, %v", untouched, err)
	}

	applied, err := principal.Rekey(ctx, db, []principal.OwnerMove{osUser, trusted}, true, participants)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	requireRekeyCounts(t, applied, 2, 2, 1, 1)
	hits, err := memories.Search(ctx, &store.MemoryQuery{OwnerID: "local-owner", Terms: "nightly backup", Limit: 10})
	if err != nil || len(hits) != 1 || hits[0].ID != "mem-owner" {
		t.Fatalf("owner search after rekey = %+v, %v", hits, err)
	}
	hits, err = memories.Search(ctx, &store.MemoryQuery{OwnerID: "discord:111", Terms: "weekly report", Limit: 10})
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

	again, err := principal.Rekey(ctx, db, []principal.OwnerMove{osUser, trusted}, true, participants)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if again.Moved() != 0 || again.Count(principal.RekeyLineProfileFactConflicts) != 1 {
		t.Fatalf("second apply = moved %d conflicts %d, want zero moves and the standing conflict", again.Moved(), again.Count(principal.RekeyLineProfileFactConflicts))
	}
}

func TestRekeyOwners_NoMatchesReportsZero(t *testing.T) {
	db := newTestDB(t)
	participants, err := rekeyParticipants(db)
	if err != nil {
		t.Fatalf("rekeyParticipants: %v", err)
	}
	report, err := principal.Rekey(t.Context(), db, []principal.OwnerMove{{From: "kuma", To: "local-owner"}}, true, participants)
	if err != nil {
		t.Fatalf("Rekey: %v", err)
	}
	requireRekeyCounts(t, report, 0, 0, 0, 0)
}

func TestRekeyOwners_RejectsInvalidMoves(t *testing.T) {
	db := newTestDB(t)
	participants, err := rekeyParticipants(db)
	if err != nil {
		t.Fatalf("rekeyParticipants: %v", err)
	}
	for name, moves := range map[string][]principal.OwnerMove{
		"empty source":     {{From: "", To: "local-owner"}},
		"empty target":     {{From: "kuma", To: " "}},
		"self move":        {{From: "local-owner", To: "local-owner"}},
		"duplicate source": {{From: "kuma", To: "local-owner"}, {From: "kuma", To: "discord:1"}},
		"chained move":     {{From: "kuma", To: "local-owner"}, {From: "local-owner", To: "x"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := principal.Rekey(t.Context(), db, moves, false, participants)
			if code, ok := fault.CodeOf[principal.ErrorCode](err); !ok || code != principal.ErrorCodeInvalidArgument {
				t.Fatalf("code = %q, %v; want invalid_argument", code, ok)
			}
		})
	}
}
