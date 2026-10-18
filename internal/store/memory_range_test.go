package store

import (
	"testing"
	"time"
)

func TestMemoryStore_ListSessionDocumentsOrdersAndFilters(t *testing.T) {
	db := newTestDB(t)
	s := NewMemoryStore(db)
	ctx := t.Context()
	seedMemorySession(t, db, "sess-1", "owner-1")
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	for i, seq := range []int64{3, 1, 2} {
		document := testMemoryDocument("mem-"+string(rune('a'+i)), "sess-1", MemoryKindEventText)
		document.FromSequence, document.ToSequence = seq, seq
		document.CreatedAt = now
		if _, err := s.UpsertDocument(ctx, document); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	summary := testMemoryDocument("mem-sum", "sess-1", MemoryKindSummary)
	summary.FromSequence, summary.ToSequence = 1, 3
	summary.CreatedAt = now
	if _, err := s.UpsertDocument(ctx, summary); err != nil {
		t.Fatalf("seed summary: %v", err)
	}
	expiredAt := now.Add(-time.Minute)
	expired := testMemoryDocument("mem-exp", "sess-1", MemoryKindSummary)
	expired.FromSequence, expired.ToSequence = 1, 1
	expired.ExpiresAt = &expiredAt
	expired.CreatedAt = now.Add(-time.Hour)
	if _, err := s.UpsertDocument(ctx, expired); err != nil {
		t.Fatalf("seed expired: %v", err)
	}
	raw, err := s.ListSessionDocumentsByKind(ctx, "owner-1", "sess-1", MemoryKindEventText, 10)
	if err != nil {
		t.Fatalf("ListSessionDocuments(): %v", err)
	}
	if len(raw) != 3 || raw[0].FromSequence != 1 || raw[1].FromSequence != 2 || raw[2].FromSequence != 3 {
		t.Fatalf("raw order = %+v, want 1,2,3", raw)
	}
	summaries, err := s.ListSessionDocumentsByKind(ctx, "owner-1", "sess-1", MemoryKindSummary, 10)
	if err != nil || len(summaries) != 1 || summaries[0].ID != "mem-sum" {
		t.Fatalf("summaries = %+v, %v; want only non-expired mem-sum", summaries, err)
	}
	limited, err := s.ListSessionDocumentsByKind(ctx, "owner-1", "sess-1", MemoryKindEventText, 2)
	if err != nil || len(limited) != 2 {
		t.Fatalf("limited = %d, %v; want 2", len(limited), err)
	}
	foreign, err := s.ListSessionDocumentsByKind(ctx, "owner-2", "sess-1", MemoryKindEventText, 10)
	if err != nil || len(foreign) != 0 {
		t.Fatalf("foreign = %d, %v; want none", len(foreign), err)
	}
	if _, err := s.ListSessionDocumentsByKind(ctx, "", "sess-1", MemoryKindEventText, 10); err == nil {
		t.Error("empty owner accepted")
	}
	if _, err := s.ListSessionDocumentsByKind(ctx, "owner-1", "sess-1", "note", 10); err == nil {
		t.Error("bad kind accepted")
	}
	if _, err := s.ListSessionDocumentsByKind(ctx, "owner-1", "sess-1", MemoryKindEventText, 0); err == nil {
		t.Error("non-positive limit accepted")
	}
}

func TestMemoryStore_DeleteSessionSummariesKeepsRaw(t *testing.T) {
	db := newTestDB(t)
	s := NewMemoryStore(db)
	ctx := t.Context()
	seedMemorySession(t, db, "sess-1", "owner-1")
	raw := testMemoryDocument("mem-raw", "sess-1", MemoryKindEventText)
	if _, err := s.UpsertDocument(ctx, raw); err != nil {
		t.Fatalf("seed raw: %v", err)
	}
	for i, id := range []string{"mem-s1", "mem-s2"} {
		summary := testMemoryDocument(id, "sess-1", MemoryKindSummary)
		summary.FromSequence, summary.ToSequence = int64(i+1), int64(i+1)
		if _, err := s.UpsertDocument(ctx, summary); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	removed, err := s.DeleteSessionSummaries(ctx, "owner-1", "sess-1")
	if err != nil || removed != 2 {
		t.Fatalf("removed = %d, %v; want 2", removed, err)
	}
	remaining, err := s.ListSessionDocumentsByKind(ctx, "owner-1", "sess-1", MemoryKindEventText, 10)
	if err != nil || len(remaining) != 1 {
		t.Fatalf("raw after delete = %d, %v; want 1", len(remaining), err)
	}
	if _, err := s.DeleteSessionSummaries(ctx, "", "sess-1"); err == nil {
		t.Error("empty owner accepted")
	}
}

func TestMemoryStore_DeleteOtherSummariesKeepsOne(t *testing.T) {
	db := newTestDB(t)
	s := NewMemoryStore(db)
	ctx := t.Context()
	seedMemorySession(t, db, "sess-1", "owner-1")
	for i, id := range []string{"mem-keep", "mem-drop"} {
		summary := testMemoryDocument(id, "sess-1", MemoryKindSummary)
		summary.FromSequence, summary.ToSequence = int64(i+1), int64(i+1)
		if _, err := s.UpsertDocument(ctx, summary); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	removed, err := s.DeleteOtherSummaries(ctx, "owner-1", "sess-1", "mem-keep")
	if err != nil || removed != 1 {
		t.Fatalf("removed = %d, %v; want 1", removed, err)
	}
	remaining, err := s.ListSessionDocumentsByKind(ctx, "owner-1", "sess-1", MemoryKindSummary, 10)
	if err != nil || len(remaining) != 1 || remaining[0].ID != "mem-keep" {
		t.Fatalf("remaining = %+v, %v; want mem-keep", remaining, err)
	}
	if _, err := s.DeleteOtherSummaries(ctx, "owner-1", "sess-1", ""); err == nil {
		t.Error("empty keep id accepted")
	}
}
