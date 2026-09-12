package store

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

func seedMemorySession(t *testing.T, db *sql.DB, sessionID, ownerID string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO session (id, owner_id, metadata_json, created_at, updated_at) VALUES (?,?,?,?,?) ON CONFLICT(id) DO NOTHING`,
		sessionID, ownerID, `{}`, "2026-09-12T12:00:00Z", "2026-09-12T12:00:00Z"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

func testMemoryDocument(id, session, kind string) *MemoryDocument {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	return &MemoryDocument{
		ID: id, OwnerID: "owner-1", SessionID: session, Kind: kind,
		FromSequence: 1, ToSequence: 1, Content: "the nightly backup finished",
		TrustLabel: "owner_input", CreatedAt: now,
	}
}

func TestMemoryStore_UpsertSearchRoundTrip(t *testing.T) {
	db := newTestDB(t)
	s := NewMemoryStore(db)
	ctx := t.Context()
	seedMemorySession(t, db, "sess-1", "owner-1")

	inserted, err := s.UpsertDocument(ctx, testMemoryDocument("mem-1", "sess-1", MemoryKindEventText))
	if err != nil {
		t.Fatalf("UpsertDocument(): %v", err)
	}
	if !inserted {
		t.Error("first upsert reported present")
	}
	inserted, err = s.UpsertDocument(ctx, testMemoryDocument("mem-1", "sess-1", MemoryKindEventText))
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if inserted {
		t.Error("identical re-projection reported insert")
	}
	hits, err := s.Search(ctx, &MemoryQuery{OwnerID: "owner-1", Terms: "backup", Limit: 10})
	if err != nil {
		t.Fatalf("Search(): %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "mem-1" {
		t.Fatalf("hits = %+v", hits)
	}
	if hits[0].Rank >= 0 {
		t.Errorf("bm25 rank = %v, want negative", hits[0].Rank)
	}
	if _, err := s.Search(ctx, &MemoryQuery{OwnerID: "owner-2", Terms: "backup", Limit: 10}); err != nil {
		t.Fatalf("foreign owner search: %v", err)
	}
	foreign, err := s.Search(ctx, &MemoryQuery{OwnerID: "owner-2", Terms: "backup", Limit: 10})
	if err != nil || len(foreign) != 0 {
		t.Errorf("foreign owner hits = %d, %v; want none", len(foreign), err)
	}
}

func TestMemoryStore_SearchRankingAndBounds(t *testing.T) {
	db := newTestDB(t)
	s := NewMemoryStore(db)
	ctx := t.Context()
	seedMemorySession(t, db, "sess-1", "owner-1")

	ids := []string{"mem-a", "mem-b", "mem-c"}
	for i, text := range []string{"backup finished nightly", "backup failed nightly", "unrelated weather chat"} {
		document := testMemoryDocument(ids[i], "sess-1", MemoryKindEventText)
		document.Content = text
		document.FromSequence = int64(i + 1)
		document.ToSequence = int64(i + 1)
		if _, err := s.UpsertDocument(ctx, document); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	hits, err := s.Search(ctx, &MemoryQuery{OwnerID: "owner-1", Terms: "backup nightly", Limit: 10})
	if err != nil {
		t.Fatalf("Search(): %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2", len(hits))
	}
	if hits[0].Rank > hits[1].Rank {
		t.Errorf("ranking not by bm25: %v then %v", hits[0].Rank, hits[1].Rank)
	}
	if hits[0].ID == hits[1].ID {
		t.Error("duplicate hit ids")
	}
	limited, err := s.Search(ctx, &MemoryQuery{OwnerID: "owner-1", Terms: "backup nightly", Limit: 1})
	if err != nil || len(limited) != 1 || limited[0].ID != hits[0].ID {
		t.Errorf("limited search = %+v, %v", limited, err)
	}
	noisy, err := s.Search(ctx, &MemoryQuery{OwnerID: "owner-1", Terms: `backup* (nightly)`, Limit: 10})
	if err != nil || len(noisy) != 2 {
		t.Errorf("syntax-neutralized search = %d, %v; want 2", len(noisy), err)
	}
	if _, err := s.Search(ctx, &MemoryQuery{OwnerID: "owner-1", Terms: "(((", Limit: 10}); err == nil {
		t.Error("term-less query accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
		t.Errorf("term-less query code = %v, %v", code, ok)
	}
}

func TestMemoryStore_SearchFilters(t *testing.T) {
	db := newTestDB(t)
	s := NewMemoryStore(db)
	ctx := t.Context()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	seedMemorySession(t, db, "sess-1", "owner-1")

	fresh := testMemoryDocument("mem-fresh", "sess-1", MemoryKindEventText)
	fresh.CreatedAt = now
	if _, err := s.UpsertDocument(ctx, fresh); err != nil {
		t.Fatalf("seed: %v", err)
	}
	stale := testMemoryDocument("mem-stale", "sess-1", MemoryKindEventText)
	stale.CreatedAt = now.Add(-time.Hour)
	if _, err := s.UpsertDocument(ctx, stale); err != nil {
		t.Fatalf("seed: %v", err)
	}
	expiredAt := now.Add(-time.Minute)
	expired := testMemoryDocument("mem-expired", "sess-1", MemoryKindEventText)
	expired.Content = "backup finished nightly"
	expired.ExpiresAt = &expiredAt
	if _, err := s.UpsertDocument(ctx, expired); err != nil {
		t.Fatalf("seed: %v", err)
	}

	before, err := s.Search(ctx, &MemoryQuery{OwnerID: "owner-1", Terms: "backup", Before: now.Add(-30 * time.Minute), Limit: 10})
	if err != nil {
		t.Fatalf("Search(): %v", err)
	}
	for _, hit := range before {
		if hit.ID == "mem-fresh" {
			t.Errorf("before-filter leaked fresh row: %+v", hit.ID)
		}
	}
	current, err := s.Search(ctx, &MemoryQuery{OwnerID: "owner-1", Terms: "backup", Limit: 10})
	if err != nil {
		t.Fatalf("Search(): %v", err)
	}
	for _, hit := range current {
		if hit.ID == "mem-expired" {
			t.Errorf("expired row returned: %+v", hit.ID)
		}
	}
	if len(current) == 0 {
		t.Error("no current rows returned")
	}
}

func TestMemoryStore_WatermarkDeleteAndExpiry(t *testing.T) {
	db := newTestDB(t)
	s := NewMemoryStore(db)
	ctx := t.Context()
	seedMemorySession(t, db, "sess-1", "owner-1")

	if _, found, err := s.ProjectionWatermark(ctx, "sess-1"); err != nil || found {
		t.Errorf("empty watermark = %v, %v", found, err)
	}
	first := testMemoryDocument("mem-1", "sess-1", MemoryKindEventText)
	first.FromSequence, first.ToSequence = 1, 3
	second := testMemoryDocument("mem-2", "sess-1", MemoryKindEventText)
	second.FromSequence, second.ToSequence = 4, 7
	for _, document := range []*MemoryDocument{first, second} {
		if _, err := s.UpsertDocument(ctx, document); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	mark, found, err := s.ProjectionWatermark(ctx, "sess-1")
	if err != nil || !found || mark != 7 {
		t.Errorf("watermark = %d, %v, %v; want 7", mark, found, err)
	}
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	summary := testMemoryDocument("mem-sum", "sess-1", MemoryKindSummary)
	summary.FromSequence, summary.ToSequence = 1, 7
	summary.ExpiresAt = &past
	if _, err := s.UpsertDocument(ctx, summary); err != nil {
		t.Fatalf("seed summary: %v", err)
	}
	pruned, err := s.DeleteExpiredSummaries(ctx, now, 10)
	if err != nil {
		t.Fatalf("DeleteExpiredSummaries(): %v", err)
	}
	if pruned != 1 {
		t.Errorf("pruned = %d, want 1", pruned)
	}
	event := testMemoryDocument("mem-event-old", "sess-1", MemoryKindEventText)
	event.FromSequence, event.ToSequence = 8, 8
	old := now.Add(-1000 * time.Hour)
	event.CreatedAt = old
	if _, err := s.UpsertDocument(ctx, event); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if pruned, err := s.DeleteExpiredSummaries(ctx, now, 10); err != nil || pruned != 0 {
		t.Errorf("event rows must follow session retention, not summary GC: %d, %v", pruned, err)
	}
	if err := s.DeleteSessionDocuments(ctx, "sess-1"); err != nil {
		t.Fatalf("DeleteSessionDocuments(): %v", err)
	}
	hits, err := s.Search(ctx, &MemoryQuery{OwnerID: "owner-1", Terms: "backup", Limit: 10})
	if err != nil || len(hits) != 0 {
		t.Errorf("rows survive session delete: %+v, %v", len(hits), err)
	}
}

func TestMemoryStore_RejectsInvalidRows(t *testing.T) {
	db := newTestDB(t)
	s := NewMemoryStore(db)
	ctx := t.Context()

	valid := func() *MemoryDocument { return testMemoryDocument("mem-1", "sess-1", MemoryKindEventText) }
	cases := []struct {
		name   string
		mutate func(*MemoryDocument)
	}{
		{"empty id", func(document *MemoryDocument) { document.ID = "" }},
		{"empty owner", func(document *MemoryDocument) { document.OwnerID = "" }},
		{"bad kind", func(document *MemoryDocument) { document.Kind = "note" }},
		{"zero from", func(document *MemoryDocument) { document.FromSequence = 0 }},
		{"inverted range", func(document *MemoryDocument) { document.ToSequence = 0 }},
		{"empty content", func(document *MemoryDocument) { document.Content = "" }},
		{"empty trust", func(document *MemoryDocument) { document.TrustLabel = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			document := valid()
			tc.mutate(document)
			if _, err := s.UpsertDocument(ctx, document); err == nil {
				t.Errorf("invalid row accepted: %+v", document)
			}
		})
	}
	if _, err := s.UpsertDocument(ctx, nil); err == nil {
		t.Error("nil document accepted")
	}
	if _, err := s.Search(ctx, nil); err == nil {
		t.Error("nil query accepted")
	}
	if _, err := s.Search(ctx, &MemoryQuery{OwnerID: "owner-1", Terms: "!!!", Limit: 10}); err == nil {
		t.Error("term-less query accepted")
	}
	if _, err := s.Search(ctx, &MemoryQuery{OwnerID: "owner-1", Terms: "backup", Limit: 0}); err == nil {
		t.Error("non-positive limit accepted")
	}
}

func TestMemoryStore_SchemaVersionThirteen(t *testing.T) {
	db := newTestDB(t)
	applied, latest, err := SchemaVersions(t.Context(), db)
	if err != nil {
		t.Fatalf("SchemaVersions(): %v", err)
	}
	if latest != 13 || applied != 13 {
		t.Errorf("schema = applied %d latest %d, want 13", applied, latest)
	}
	var ftsSQL string
	if err := db.QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master WHERE name = 'memory_document_fts'`).Scan(&ftsSQL); err != nil {
		t.Fatalf("fts table: %v", err)
	}
	if !strings.Contains(ftsSQL, "unicode61") {
		t.Errorf("fts tokenizer wrong: %s", ftsSQL)
	}
}
