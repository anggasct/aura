package profile

import (
	"testing"
	"time"
)

func seedActiveProfileFact(t *testing.T, s *SQLiteStore, id, category, key, value string, verified bool, updatedAt time.Time) {
	t.Helper()
	fact := &Fact{
		ID:               id,
		OwnerID:          "owner-1",
		Category:         category,
		Key:              key,
		Value:            value,
		ValueDigest:      ValueDigest(value),
		Status:           StatusActive,
		Origin:           OriginOwner,
		OwnerVerified:    verified,
		Confidence:       0.9,
		ConfidencePolicy: ConfidencePolicyV1,
		CreatedAt:        updatedAt,
		UpdatedAt:        updatedAt,
	}
	if err := s.InsertFact(t.Context(), fact); err != nil {
		t.Fatalf("InsertFact(%s): %v", id, err)
	}
}

func TestProfileSearchUsesTwoStageFTSAndRecency(t *testing.T) {
	s, _ := newTestSQLiteStore(t)
	ctx := t.Context()
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

	seedActiveProfileFact(t, s, "pf-exact-old", "preference", "travel", "rencana liburan Jepang Kyoto", true, now.Add(-120*24*time.Hour))
	seedActiveProfileFact(t, s, "pf-partial-new", "preference", "hobby", "liburan singkat pantai", true, now.Add(-24*time.Hour))
	seedActiveProfileFact(t, s, "pf-eq-old", "tool", "editor-a", "neovim configuration alpha", true, now.Add(-120*24*time.Hour))
	seedActiveProfileFact(t, s, "pf-eq-new", "tool", "editor-b", "neovim configuration bravo", true, now.Add(-24*time.Hour))
	seedActiveProfileFact(t, s, "pf-unverified", "tool", "editor-c", "neovim configuration charlie", false, now.Add(-time.Hour))

	natural, err := s.SearchFacts(ctx, "owner-1", "", "eh minggu lalu kita bahas rencana liburan ke Jepang kan?", false, 10, now)
	if err != nil {
		t.Fatalf("SearchFacts(natural): %v", err)
	}
	if len(natural) == 0 || natural[0].ID != "pf-exact-old" {
		t.Fatalf("SearchFacts(natural) = %+v, want pf-exact-old first via OR fallback", natural)
	}

	tiered, err := s.SearchFacts(ctx, "owner-1", "preference", "rencana liburan", false, 10, now)
	if err != nil {
		t.Fatalf("SearchFacts(tiered): %v", err)
	}
	if len(tiered) != 2 || tiered[0].ID != "pf-exact-old" || tiered[1].ID != "pf-partial-new" {
		t.Fatalf("tiered order = %+v, want Stage 1 pf-exact-old ahead of newer Stage 2 pf-partial-new", tiered)
	}

	recency, err := s.SearchFacts(ctx, "owner-1", "tool", "neovim configuration", true, 10, now)
	if err != nil {
		t.Fatalf("SearchFacts(recency): %v", err)
	}
	if len(recency) != 2 || recency[0].ID != "pf-eq-new" || recency[1].ID != "pf-eq-old" {
		t.Fatalf("recency verified order = %+v, want [pf-eq-new, pf-eq-old] without unverified fact", recency)
	}
}
