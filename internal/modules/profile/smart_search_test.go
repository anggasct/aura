package profile

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
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

func TestProfileSearchExactSubsecondExpiryAndNoStarvation(t *testing.T) {
	s, _ := newTestSQLiteStore(t)
	ctx := t.Context()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	justExpired := now.Add(-500 * time.Millisecond)
	validSubsecondExpiry := now.Add(500 * time.Millisecond)

	for i := range 60 {
		exp := justExpired
		fact := &Fact{
			ID:               fmt.Sprintf("pf-exp-%02d", i),
			OwnerID:          "owner-1",
			Category:         "project",
			Key:              fmt.Sprintf("k-%02d", i),
			Value:            "starvation guard alpha",
			ValueDigest:      ValueDigest(fmt.Sprintf("starvation-%02d", i)),
			Status:           StatusActive,
			Origin:           OriginOwner,
			OwnerVerified:    true,
			Confidence:       0.99,
			ConfidencePolicy: ConfidencePolicyV1,
			ExpiresAt:        &exp,
			CreatedAt:        now.Add(-time.Hour),
			UpdatedAt:        now.Add(-time.Hour),
		}
		if err := s.InsertFact(ctx, fact); err != nil {
			t.Fatalf("InsertFact(%s): %v", fact.ID, err)
		}
	}
	validFact := &Fact{
		ID:               "pf-valid-subsec",
		OwnerID:          "owner-1",
		Category:         "project",
		Key:              "subsec",
		Value:            "starvation guard alpha",
		ValueDigest:      ValueDigest("starvation-valid"),
		Status:           StatusActive,
		Origin:           OriginOwner,
		OwnerVerified:    true,
		Confidence:       0.80,
		ConfidencePolicy: ConfidencePolicyV1,
		ExpiresAt:        &validSubsecondExpiry,
		CreatedAt:        now.Add(-time.Minute),
		UpdatedAt:        now.Add(-time.Minute),
	}
	if err := s.InsertFact(ctx, validFact); err != nil {
		t.Fatalf("InsertFact(valid): %v", err)
	}
	partialFact := &Fact{
		ID:               "pf-partial-or",
		OwnerID:          "owner-1",
		Category:         "project",
		Key:              "partial",
		Value:            "starvation only",
		ValueDigest:      ValueDigest("starvation-partial"),
		Status:           StatusActive,
		Origin:           OriginOwner,
		OwnerVerified:    true,
		Confidence:       0.70,
		ConfidencePolicy: ConfidencePolicyV1,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := s.InsertFact(ctx, partialFact); err != nil {
		t.Fatalf("InsertFact(partial): %v", err)
	}

	emptyHits, err := s.SearchFacts(ctx, "owner-1", "project", "", false, 1, now)
	if err != nil {
		t.Fatalf("SearchFacts(empty): %v", err)
	}
	if len(emptyHits) != 1 || emptyHits[0].ID != "pf-valid-subsec" {
		t.Fatalf("empty query SearchFacts = %+v, want pf-valid-subsec with .5Z expiry past 60 just-expired rows", emptyHits)
	}

	ftsHits, err := s.SearchFacts(ctx, "owner-1", "project", "starvation guard", false, 1, now)
	if err != nil {
		t.Fatalf("SearchFacts(fts): %v", err)
	}
	if len(ftsHits) != 1 || ftsHits[0].ID != "pf-valid-subsec" {
		t.Fatalf("FTS SearchFacts = %+v, want Stage 1 pf-valid-subsec without starvation or OR fallback", ftsHits)
	}

	_, err = s.SearchFacts(ctx, "owner-1", "", strings.Repeat("a", 501), false, 10, now)
	if !fault.HasCode(err, ErrorCodeInvalidArgument) || !strings.Contains(err.Error(), "exceeds length bounds") {
		t.Fatalf("overlong query err = %v, want invalid_argument with length bounds message", err)
	}
}
