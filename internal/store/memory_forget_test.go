package store

import (
	"testing"
)

func seedOwnedDocument(t *testing.T, s MemoryStore, id, owner, session, content string, seq int64) {
	t.Helper()
	document := testMemoryDocument(id, session, MemoryKindEventText)
	document.OwnerID = owner
	document.Content = content
	document.FromSequence = seq
	document.ToSequence = seq
	if _, err := s.UpsertDocument(t.Context(), document); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func TestMemoryStore_OwnedDeletesAreFTSConsistent(t *testing.T) {
	db := newTestDB(t)
	s := NewMemoryStore(db)
	ctx := t.Context()
	seedMemorySession(t, db, "sess-a", "owner-1")
	seedMemorySession(t, db, "sess-b", "owner-1")
	seedMemorySession(t, db, "sess-c", "owner-2")
	seedOwnedDocument(t, s, "mem-a1", "owner-1", "sess-a", "alpha recording one", 1)
	seedOwnedDocument(t, s, "mem-a2", "owner-1", "sess-a", "alpha recording two", 2)
	seedOwnedDocument(t, s, "mem-b1", "owner-1", "sess-b", "beta recording one", 1)
	seedOwnedDocument(t, s, "mem-c1", "owner-2", "sess-c", "alpha recording foreign", 1)

	owned, err := s.ListSessionDocuments(ctx, "owner-1", "sess-a", 10)
	if err != nil || len(owned) != 2 {
		t.Fatalf("list sess-a = %d, %v; want 2", len(owned), err)
	}
	if total, err := s.CountSessionDocuments(ctx, "sess-a"); err != nil || total != 2 {
		t.Fatalf("count sess-a = %d, %v; want 2", total, err)
	}
	owned, err = s.ListSessionDocuments(ctx, "owner-1", "sess-a", 10)
	if err != nil || len(owned) != 2 {
		t.Fatalf("list sess-a = %d, %v; want 2", len(owned), err)
	}
	ids := make([]string, 0, len(owned))
	for _, doc := range owned {
		ids = append(ids, doc.ID)
	}
	deleted, err := s.DeleteDocumentsByIDsOwned(ctx, "owner-1", ids)
	if err != nil || deleted != 2 {
		t.Fatalf("owned delete = %d, %v; want 2", deleted, err)
	}
	hits, err := s.Search(ctx, &MemoryQuery{OwnerID: "owner-1", Terms: "alpha", Limit: 10})
	if err != nil {
		t.Fatalf("search alpha: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("ghost hits after owned delete: %+v", hits)
	}
	kept, err := s.Search(ctx, &MemoryQuery{OwnerID: "owner-1", Terms: "beta", Limit: 10})
	if err != nil || len(kept) != 1 || kept[0].ID != "mem-b1" {
		t.Errorf("sess-b untouched = %+v, %v; want mem-b1", kept, err)
	}
	foreign, err := s.Search(ctx, &MemoryQuery{OwnerID: "owner-2", Terms: "alpha", Limit: 10})
	if err != nil || len(foreign) != 1 || foreign[0].ID != "mem-c1" {
		t.Errorf("foreign untouched = %+v, %v; want mem-c1", foreign, err)
	}
}

func TestMemoryStore_OwnedDeletesLeaveForeignInSameSession(t *testing.T) {
	db := newTestDB(t)
	s := NewMemoryStore(db)
	ctx := t.Context()
	seedMemorySession(t, db, "sess-mixed", "owner-1")
	seedOwnedDocument(t, s, "mem-own", "owner-1", "sess-mixed", "alpha owned recording", 1)
	seedOwnedDocument(t, s, "mem-foreign", "owner-2", "sess-mixed", "alpha foreign recording", 2)

	owned, err := s.ListSessionDocuments(ctx, "owner-1", "sess-mixed", 10)
	if err != nil || len(owned) != 1 {
		t.Fatalf("list owned = %d, %v; want 1", len(owned), err)
	}
	ids := make([]string, 0, len(owned))
	for _, doc := range owned {
		ids = append(ids, doc.ID)
	}
	deleted, err := s.DeleteDocumentsByIDsOwned(ctx, "owner-1", ids)
	if err != nil || deleted != 1 {
		t.Fatalf("owned delete = %d, %v; want 1", deleted, err)
	}
	if _, found, err := s.GetDocument(ctx, "mem-own"); err != nil || found {
		t.Errorf("owned doc survives: %v, %v", found, err)
	}
	document, found, err := s.GetDocument(ctx, "mem-foreign")
	if err != nil || !found || document.OwnerID != "owner-2" {
		t.Errorf("foreign doc = %+v, %v, %v; want preserved", document, found, err)
	}
	if total, err := s.CountSessionDocuments(ctx, "sess-mixed"); err != nil || total != 1 {
		t.Errorf("count mixed = %d, %v; want 1", total, err)
	}
}

func TestMemoryStore_GetListOwnerAndDeleteByIDs(t *testing.T) {
	db := newTestDB(t)
	s := NewMemoryStore(db)
	ctx := t.Context()
	seedMemorySession(t, db, "sess-a", "owner-1")
	seedMemorySession(t, db, "sess-b", "owner-1")
	seedOwnedDocument(t, s, "mem-a1", "owner-1", "sess-a", "alpha first recording", 1)
	seedOwnedDocument(t, s, "mem-b1", "owner-1", "sess-b", "beta second recording", 1)

	document, found, err := s.GetDocument(ctx, "mem-a1")
	if err != nil || !found || document.SessionID != "sess-a" {
		t.Fatalf("get mem-a1 = %+v, %v, %v", document, found, err)
	}
	if _, found, err := s.GetDocument(ctx, "mem-missing"); err != nil || found {
		t.Errorf("missing doc = %v, %v; want absent", found, err)
	}
	owned, err := s.ListOwnerDocuments(ctx, "owner-1", 10)
	if err != nil || len(owned) != 2 {
		t.Fatalf("list owner = %d, %v; want 2", len(owned), err)
	}
	deleted, err := s.DeleteDocumentsByIDsOwned(ctx, "owner-1", []string{"mem-a1"})
	if err != nil || deleted != 1 {
		t.Fatalf("delete by ids = %d, %v; want 1", deleted, err)
	}
	hits, err := s.Search(ctx, &MemoryQuery{OwnerID: "owner-1", Terms: "alpha", Limit: 10})
	if err != nil || len(hits) != 0 {
		t.Errorf("ghost after id delete: %+v, %v", hits, err)
	}
	kept, err := s.Search(ctx, &MemoryQuery{OwnerID: "owner-1", Terms: "beta", Limit: 10})
	if err != nil || len(kept) != 1 {
		t.Errorf("beta hits = %d, %v; want 1", len(kept), err)
	}
	if deleted, err := s.DeleteDocumentsByIDsOwned(ctx, "owner-2", []string{"mem-b1"}); err != nil || deleted != 0 {
		t.Errorf("foreign id delete = %d, %v; want 0", deleted, err)
	}
}

func TestMemoryStore_RejectsBadOwnedArguments(t *testing.T) {
	db := newTestDB(t)
	s := NewMemoryStore(db)
	ctx := t.Context()
	if _, err := s.ListSessionDocuments(ctx, "", "sess-a", 10); err == nil {
		t.Error("empty owner accepted")
	}
	if _, err := s.ListSessionDocuments(ctx, "owner-1", "sess-a", 0); err == nil {
		t.Error("non-positive limit accepted")
	}
	if _, err := s.ListOwnerDocuments(ctx, "", 10); err == nil {
		t.Error("empty owner accepted")
	}
	if _, err := s.DeleteDocumentsByIDsOwned(ctx, "owner-1", []string{""}); err == nil {
		t.Error("empty id accepted")
	}
}
