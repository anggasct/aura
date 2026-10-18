package memory

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func seedStoredDocument(t *testing.T, s *Service, id, owner, session, content string, seq int64) {
	t.Helper()
	now := time.Now().UTC()
	document := &StoredDocument{
		ID: id, OwnerID: owner, SessionID: session, Kind: KindEventText,
		FromSequence: seq, ToSequence: seq, Content: content,
		TrustLabel: "owner_input", CreatedAt: now,
	}
	if _, err := s.documents.UpsertDocument(t.Context(), document); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func TestForgetBySessionDeletesRowsAndFTS(t *testing.T) {
	service := testService()
	seedStoredDocument(t, service, "mem-a1", "owner-1", "sess-a", "alpha recording one", 1)
	seedStoredDocument(t, service, "mem-a2", "owner-1", "sess-a", "alpha recording two", 2)
	seedStoredDocument(t, service, "mem-b1", "owner-1", "sess-b", "beta recording one", 1)
	seedStoredDocument(t, service, "mem-c1", "owner-2", "sess-c", "alpha foreign recording", 1)

	preview, deleted, err := service.ForgetBySession(t.Context(), "owner-1", "sess-a", 10, false)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if deleted != 0 || len(preview) != 2 {
		t.Fatalf("dry-run = %d deleted, %d preview; want 0, 2", deleted, len(preview))
	}
	afterDry, err := service.Recall(t.Context(), &RecallRequest{OwnerID: "owner-1", SessionID: "sess-a", Query: "alpha"})
	if err != nil || len(afterDry) != 2 {
		t.Fatalf("dry-run wrote: %d, %v", len(afterDry), err)
	}
	preview, deleted, err = service.ForgetBySession(t.Context(), "owner-1", "sess-a", 10, true)
	if err != nil || deleted != 2 || len(preview) != 2 {
		t.Fatalf("apply = %d, %d, %v; want 2, 2", deleted, len(preview), err)
	}
	after, err := service.Recall(t.Context(), &RecallRequest{OwnerID: "owner-1", SessionID: "sess-a", Query: "alpha"})
	if err != nil || len(after) != 0 {
		t.Errorf("sess-a survives: %+v, %v", after, err)
	}
	kept, err := service.Recall(t.Context(), &RecallRequest{OwnerID: "owner-1", SessionID: "sess-b", Query: "beta"})
	if err != nil || len(kept) != 1 {
		t.Errorf("sess-b untouched = %d, %v", len(kept), err)
	}
	foreign, err := service.Recall(t.Context(), &RecallRequest{OwnerID: "owner-2", SessionID: "sess-c", Query: "alpha"})
	if err != nil || len(foreign) != 1 {
		t.Errorf("foreign untouched = %d, %v", len(foreign), err)
	}
}

func TestForgetBySessionApplyMatchesPreviewedSet(t *testing.T) {
	service := testService()
	for seq := int64(1); seq <= 12; seq++ {
		seedStoredDocument(t, service, fmt.Sprintf("mem-a%02d", seq), "owner-1", "sess-a", "alpha recording", seq)
	}
	preview, deleted, err := service.ForgetBySession(t.Context(), "owner-1", "sess-a", 10, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(preview) != 10 {
		t.Fatalf("preview = %d, want 10", len(preview))
	}
	if deleted != len(preview) {
		t.Fatalf("deleted = %d, want %d (previewed set)", deleted, len(preview))
	}
	remaining, err := service.Recall(t.Context(), &RecallRequest{OwnerID: "owner-1", SessionID: "sess-a", Query: "alpha"})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(remaining) != 2 {
		t.Fatalf("remaining = %d, want 2", len(remaining))
	}
	for _, doc := range remaining {
		if doc.ID != "mem-a11" && doc.ID != "mem-a12" {
			t.Errorf("unexpected survivor %q", doc.ID)
		}
	}
}

func TestForgetByIDAndQueryScoped(t *testing.T) {
	service := testService()
	seedStoredDocument(t, service, "mem-a1", "owner-1", "sess-a", "alpha recording one", 1)
	seedStoredDocument(t, service, "mem-a2", "owner-1", "sess-a", "alpha recording two", 2)

	preview, deleted, err := service.ForgetByID(t.Context(), "owner-1", "mem-a1", false)
	if err != nil || deleted != 0 || len(preview) != 1 {
		t.Fatalf("id dry-run = %d, %d, %v", deleted, len(preview), err)
	}
	_, deleted, err = service.ForgetByID(t.Context(), "owner-1", "mem-a1", true)
	if err != nil || deleted != 1 {
		t.Fatalf("id apply = %d, %v", deleted, err)
	}
	remaining, err := service.Recall(t.Context(), &RecallRequest{OwnerID: "owner-1", SessionID: "sess-a", Query: "alpha"})
	if err != nil || len(remaining) != 1 || remaining[0].ID != "mem-a2" {
		t.Errorf("remaining = %+v, %v; want mem-a2", remaining, err)
	}
	preview, deleted, err = service.ForgetByQuery(t.Context(), "owner-1", "sess-a", "alpha", 10, false)
	if err != nil || deleted != 0 || len(preview) != 1 {
		t.Fatalf("query dry-run = %d, %d, %v", deleted, len(preview), err)
	}
	_, deleted, err = service.ForgetByQuery(t.Context(), "owner-1", "sess-a", "alpha", 10, true)
	if err != nil || deleted != 1 {
		t.Fatalf("query apply = %d, %v", deleted, err)
	}
	after, err := service.Recall(t.Context(), &RecallRequest{OwnerID: "owner-1", SessionID: "sess-a", Query: "alpha"})
	if err != nil || len(after) != 0 {
		t.Errorf("query forget left %+v, %v", after, err)
	}
}

func TestForgetCrossOwnerRejectedWithZeroWrites(t *testing.T) {
	service := testService()
	seedStoredDocument(t, service, "mem-a1", "owner-1", "sess-a", "alpha recording one", 1)
	seedStoredDocument(t, service, "mem-c1", "owner-2", "sess-c", "alpha foreign recording", 1)

	if _, _, err := service.ForgetBySession(t.Context(), "owner-2", "sess-a", 10, false); err == nil {
		t.Error("cross-owner session preview accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeForbidden {
		t.Errorf("session code = %v, %v; want forbidden", code, ok)
	}
	if _, _, err := service.ForgetBySession(t.Context(), "owner-2", "sess-a", 10, true); err == nil {
		t.Error("cross-owner session apply accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeForbidden {
		t.Errorf("session apply code = %v, %v", code, ok)
	}
	if _, _, err := service.ForgetByID(t.Context(), "owner-2", "mem-a1", true); err == nil {
		t.Error("cross-owner id accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeForbidden {
		t.Errorf("id code = %v, %v", code, ok)
	}
	kept, err := service.Recall(t.Context(), &RecallRequest{OwnerID: "owner-1", SessionID: "sess-a", Query: "alpha"})
	if err != nil || len(kept) != 1 {
		t.Errorf("zero-write violated: %d, %v", len(kept), err)
	}
}

func TestForgetNonexistentIsEmptyWithoutError(t *testing.T) {
	service := testService()
	seedStoredDocument(t, service, "mem-a1", "owner-1", "sess-a", "alpha recording one", 1)

	preview, deleted, err := service.ForgetBySession(t.Context(), "owner-1", "sess-missing", 10, true)
	if err != nil || deleted != 0 || len(preview) != 0 {
		t.Errorf("missing session = %d, %d, %v; want empty", deleted, len(preview), err)
	}
	preview, deleted, err = service.ForgetByID(t.Context(), "owner-1", "mem-missing", true)
	if err != nil || deleted != 0 || len(preview) != 0 {
		t.Errorf("missing id = %d, %d, %v", deleted, len(preview), err)
	}
	preview, deleted, err = service.ForgetByQuery(t.Context(), "owner-1", "sess-a", "zebra", 10, true)
	if err != nil || deleted != 0 || len(preview) != 0 {
		t.Errorf("missing query = %d, %d, %v", deleted, len(preview), err)
	}
}

func TestInspectProvenanceAndScreening(t *testing.T) {
	service := testService()
	now := time.Now().UTC()
	expires := now.Add(time.Hour)
	document := &StoredDocument{
		ID: "mem-a1", OwnerID: "owner-1", SessionID: "sess-a", Kind: KindEventText,
		FromSequence: 3, ToSequence: 5, Content: "alpha recording one",
		TrustLabel: "owner_input", PromptVersion: "memory-summary-v1",
		CreatedAt: now, ExpiresAt: &expires,
	}
	if _, err := service.documents.UpsertDocument(t.Context(), document); err != nil {
		t.Fatalf("seed: %v", err)
	}
	leaky := &StoredDocument{
		ID: "mem-leak", OwnerID: "owner-1", SessionID: "sess-a", Kind: KindEventText,
		FromSequence: 6, ToSequence: 6, Content: "the key is sk-live-canary do not share",
		TrustLabel: "owner_input", CreatedAt: now,
	}
	if _, err := service.documents.UpsertDocument(t.Context(), leaky); err != nil {
		t.Fatalf("seed leaky: %v", err)
	}
	documents, err := service.Inspect(t.Context(), "owner-1", "sess-a", "", 10)
	if err != nil {
		t.Fatalf("Inspect(): %v", err)
	}
	if len(documents) != 2 {
		t.Fatalf("inspected = %d, want 2", len(documents))
	}
	var found *StoredDocument
	for i := range documents {
		if documents[i].ID == "mem-a1" {
			found = &documents[i]
		}
		if strings.Contains(documents[i].Content, "sk-live-canary") {
			t.Errorf("secret leaked in inspect: %q", documents[i].ID)
		}
	}
	if found == nil {
		t.Fatal("mem-a1 missing")
	}
	if found.SessionID != "sess-a" || found.FromSequence != 3 || found.ToSequence != 5 {
		t.Errorf("provenance = %+v", found)
	}
	if found.TrustLabel != "owner_input" || found.PromptVersion != "memory-summary-v1" {
		t.Errorf("trust/prompt = %+v", found)
	}
	if found.ExpiresAt == nil || !found.ExpiresAt.After(now) {
		t.Errorf("expiry = %+v", found.ExpiresAt)
	}
	queried, err := service.Inspect(t.Context(), "owner-1", "sess-a", "alpha", 10)
	if err != nil || len(queried) != 1 || queried[0].ID != "mem-a1" {
		t.Errorf("query inspect = %+v, %v", queried, err)
	}
	foreign, err := service.Inspect(t.Context(), "owner-2", "sess-a", "", 10)
	if err != nil || len(foreign) != 0 {
		t.Errorf("cross-owner inspect = %d, %v; want none", len(foreign), err)
	}
}

func TestPurgeExpiredIsBounded(t *testing.T) {
	service := testService()
	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	for seq, id := range []string{"mem-s1", "mem-s2", "mem-s3"} {
		expires := past
		document := &StoredDocument{
			ID: id, OwnerID: "owner-1", SessionID: "sess-a", Kind: KindSummary,
			FromSequence: int64(seq + 1), ToSequence: int64(seq + 1), Content: "old summary",
			TrustLabel: "derived_untrusted", CreatedAt: past, ExpiresAt: &expires,
		}
		if _, err := service.documents.UpsertDocument(t.Context(), document); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	pruned, err := service.PurgeExpired(t.Context(), now, 2)
	if err != nil || pruned != 2 {
		t.Fatalf("bounded purge = %d, %v; want 2", pruned, err)
	}
	pruned, err = service.PurgeExpired(t.Context(), now, 10)
	if err != nil || pruned != 1 {
		t.Fatalf("second purge = %d, %v; want 1", pruned, err)
	}
	if _, err := service.PurgeExpired(t.Context(), now, 0); err == nil {
		t.Error("non-positive purge accepted")
	}
	if _, err := service.PurgeExpired(t.Context(), now, 501); err == nil {
		t.Error("unbounded purge accepted")
	}
}
