package store

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func testBroadcastItem(id, producer, key string) *BroadcastItem {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	return &BroadcastItem{
		ID:               id,
		Producer:         producer,
		IdempotencyKey:   key,
		ContentDigest:    "digest-" + key,
		Priority:         BroadcastPriorityInfo,
		DestinationAlias: "default",
		ContentJSON:      `{"text":"hello"}`,
		State:            BroadcastStateScheduled,
		NotBefore:        now,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
}

func TestBroadcastStore_InsertLoadRoundTrip(t *testing.T) {
	db := newTestDB(t)
	s := NewBroadcastStore(db)
	ctx := t.Context()

	item := testBroadcastItem("bcst-1", "cron", "key-1")
	stored, replayed, err := s.InsertItem(ctx, item)
	if err != nil {
		t.Fatalf("InsertItem(): %v", err)
	}
	if replayed {
		t.Error("first insert reported replay")
	}
	if stored.ID != "bcst-1" || stored.State != BroadcastStateScheduled {
		t.Errorf("stored item mismatch: %+v", stored)
	}

	loaded, err := s.Item(ctx, "bcst-1")
	if err != nil {
		t.Fatalf("Item(): %v", err)
	}
	if loaded.Producer != "cron" || loaded.ContentJSON != `{"text":"hello"}` || loaded.AttemptCount != 0 {
		t.Errorf("loaded item mismatch: %+v", loaded)
	}
	if !loaded.NotBefore.Equal(item.NotBefore) || !loaded.CreatedAt.Equal(item.CreatedAt) {
		t.Errorf("timestamps not preserved: %+v", loaded)
	}
	if loaded.EffectID != "" || loaded.DigestParentID != "" {
		t.Errorf("nullable links not empty: %+v", loaded)
	}

	byKey, found, err := s.ItemByKey(ctx, "cron", "key-1")
	if err != nil || !found || byKey.ID != "bcst-1" {
		t.Errorf("ItemByKey() = %+v, %v, %v; want bcst-1", byKey.ID, found, err)
	}
	if _, found, err := s.ItemByKey(ctx, "cron", "missing"); err != nil || found {
		t.Errorf("ItemByKey(missing) = %v, %v; want not found", found, err)
	}
	if _, err := s.Item(ctx, "missing"); err == nil {
		t.Error("Item(missing) accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeBroadcastNotFound {
		t.Errorf("Item(missing) code = %v, %v; want broadcast_not_found", code, ok)
	}
}

func TestBroadcastStore_IdenticalReplayReturnsID(t *testing.T) {
	db := newTestDB(t)
	s := NewBroadcastStore(db)
	ctx := t.Context()

	first, replayed, err := s.InsertItem(ctx, testBroadcastItem("bcst-1", "cron", "key-1"))
	if err != nil || replayed {
		t.Fatalf("first insert = %+v, %v, %v", first.ID, replayed, err)
	}
	second, replayed, err := s.InsertItem(ctx, testBroadcastItem("bcst-2", "cron", "key-1"))
	if err != nil {
		t.Fatalf("replay insert: %v", err)
	}
	if !replayed {
		t.Error("identical replay did not report replay")
	}
	if second.ID != "bcst-1" {
		t.Errorf("replay ID = %q, want bcst-1", second.ID)
	}
}

func TestBroadcastStore_MismatchedReuseConflicts(t *testing.T) {
	db := newTestDB(t)
	s := NewBroadcastStore(db)
	ctx := t.Context()

	if _, _, err := s.InsertItem(ctx, testBroadcastItem("bcst-1", "cron", "key-1")); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	changed := testBroadcastItem("bcst-2", "cron", "key-1")
	changed.ContentDigest = "digest-changed"
	if _, _, err := s.InsertItem(ctx, changed); err == nil {
		t.Fatal("mismatched reuse accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeBroadcastConflict {
		t.Errorf("mismatch code = %v, %v; want broadcast_conflict", code, ok)
	}
}

func TestBroadcastStore_RejectsInvalidRows(t *testing.T) {
	db := newTestDB(t)
	s := NewBroadcastStore(db)
	ctx := t.Context()

	valid := func() *BroadcastItem { return testBroadcastItem("bcst-1", "cron", "key-1") }
	cases := []struct {
		name   string
		mutate func(*BroadcastItem)
	}{
		{"empty id", func(item *BroadcastItem) { item.ID = "" }},
		{"empty producer", func(item *BroadcastItem) { item.Producer = "" }},
		{"empty key", func(item *BroadcastItem) { item.IdempotencyKey = "" }},
		{"bad priority", func(item *BroadcastItem) { item.Priority = "critical" }},
		{"empty destination", func(item *BroadcastItem) { item.DestinationAlias = "" }},
		{"non-json content", func(item *BroadcastItem) { item.ContentJSON = "not json" }},
		{"bad state", func(item *BroadcastItem) { item.State = "flying" }},
		{"negative attempts", func(item *BroadcastItem) { item.AttemptCount = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := valid()
			tc.mutate(item)
			if _, _, err := s.InsertItem(ctx, item); err == nil {
				t.Errorf("invalid row accepted: %+v", item)
			}
		})
	}
	if _, _, err := s.InsertItem(ctx, nil); err == nil {
		t.Error("nil item accepted")
	}
}

func TestBroadcastStore_ConcurrentSameKeySingleID(t *testing.T) {
	db := newTestDB(t)
	s := NewBroadcastStore(db)
	ctx := t.Context()

	const writers = 8
	ids := make([]string, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stored, _, err := s.InsertItem(ctx, testBroadcastItem("bcst-new", "cron", "shared-key"))
			if err != nil {
				t.Errorf("concurrent insert: %v", err)
				return
			}
			ids[i] = stored.ID
		}()
	}
	wg.Wait()
	for i := range writers {
		if ids[i] == "" {
			t.Fatalf("writer %d recorded no ID", i)
		}
		if ids[i] != ids[0] {
			t.Fatalf("IDs diverged: %q vs %q", ids[i], ids[0])
		}
	}
}

func TestBroadcastStore_RejectsCorruptRow(t *testing.T) {
	db := newTestDB(t)
	s := NewBroadcastStore(db)
	ctx := t.Context()

	if _, _, err := s.InsertItem(ctx, testBroadcastItem("bcst-1", "cron", "key-1")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE broadcast_item SET not_before = 'not-a-time' WHERE id = 'bcst-1'`); err != nil {
		t.Fatalf("corrupt timestamp: %v", err)
	}
	if _, err := s.Item(ctx, "bcst-1"); err == nil {
		t.Error("corrupt timestamp accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeBroadcastInvalid {
		t.Errorf("corrupt row code = %v, %v; want broadcast_invalid", code, ok)
	}
}

func TestBroadcastStore_SchemaVersion(t *testing.T) {
	db := newTestDB(t)
	applied, latest, err := SchemaVersions(t.Context(), db)
	if err != nil {
		t.Fatalf("SchemaVersions(): %v", err)
	}
	if latest != 10 {
		t.Errorf("latest schema = %d, want 10", latest)
	}
	if applied != 10 {
		t.Errorf("applied schema = %d, want 10", applied)
	}
	var ddl string
	if err := db.QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master WHERE name = 'broadcast_item'`).Scan(&ddl); err != nil {
		t.Fatalf("broadcast_item DDL: %v", err)
	}
	var indexDDL string
	if err := db.QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master WHERE name = 'broadcast_due_idx'`).Scan(&indexDDL); err != nil {
		t.Fatalf("broadcast_due_idx: %v", err)
	}
	if !strings.Contains(ddl, "UNIQUE(producer, idempotency_key)") {
		t.Error("broadcast_item misses the idempotency uniqueness constraint")
	}
}
