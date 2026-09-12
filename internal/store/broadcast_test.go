package store

import (
	"database/sql"
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

func digestParentFixture(id string) *BroadcastItem {
	now := time.Date(2026, 9, 13, 7, 0, 0, 0, time.UTC)
	return &BroadcastItem{
		ID:               id,
		Producer:         "system:digest",
		IdempotencyKey:   "digest:2026-09-13T07:00:00Z:default:info:0",
		ContentDigest:    "digest-parent",
		Priority:         BroadcastPriorityInfo,
		DestinationAlias: "default",
		ContentJSON:      `{"count":2}`,
		State:            BroadcastStateScheduled,
		NotBefore:        now,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
}

func TestBroadcastStore_CreateDigestLinksChildren(t *testing.T) {
	db := newTestDB(t)
	s := NewBroadcastStore(db)
	ctx := t.Context()

	held := testBroadcastItem("bcst-1", "cron", "key-1")
	held.State = BroadcastStateHeld
	if _, _, err := s.InsertItem(ctx, held); err != nil {
		t.Fatalf("seed child: %v", err)
	}
	held2 := testBroadcastItem("bcst-2", "cron", "key-2")
	held2.State = BroadcastStateHeld
	if _, _, err := s.InsertItem(ctx, held2); err != nil {
		t.Fatalf("seed child: %v", err)
	}

	parent, replayed, err := s.CreateDigest(ctx, digestParentFixture("bcst-dg-1"), []string{"bcst-1", "bcst-2"})
	if err != nil {
		t.Fatalf("CreateDigest(): %v", err)
	}
	if replayed || parent.ID != "bcst-dg-1" {
		t.Errorf("digest = %+v, %v", parent.ID, replayed)
	}
	for _, id := range []string{"bcst-1", "bcst-2"} {
		child, err := s.Item(ctx, id)
		if err != nil {
			t.Fatalf("child %s: %v", id, err)
		}
		if child.State != BroadcastStateCancelled || child.DigestParentID != "bcst-dg-1" {
			t.Errorf("child %s not linked: %+v", id, child)
		}
	}

	same, replayed, err := s.CreateDigest(ctx, digestParentFixture("bcst-dg-9"), []string{"bcst-1"})
	if err != nil || !replayed || same.ID != "bcst-dg-1" {
		t.Errorf("digest replay = %+v, %v, %v; want existing parent replayed", same.ID, replayed, err)
	}
}

func TestBroadcastStore_CreateDigestRejectsUnheld(t *testing.T) {
	db := newTestDB(t)
	s := NewBroadcastStore(db)
	ctx := t.Context()

	scheduled := testBroadcastItem("bcst-1", "cron", "key-1")
	if _, _, err := s.InsertItem(ctx, scheduled); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, err := s.CreateDigest(ctx, digestParentFixture("bcst-dg-1"), []string{"bcst-1"}); err == nil {
		t.Fatal("non-held child linked")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeBroadcastConflict {
		t.Errorf("code = %v, %v; want broadcast_conflict", code, ok)
	}
	if _, err := s.Item(ctx, "bcst-dg-1"); err == nil {
		t.Error("parent persisted despite child failure")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeBroadcastNotFound {
		t.Errorf("parent lookup = %v, %v; want not found", code, ok)
	}
	if _, _, err := s.CreateDigest(ctx, digestParentFixture("bcst-dg-2"), nil); err == nil {
		t.Error("empty children accepted")
	}
}

func TestBroadcastStore_DispatchSlotsArePaced(t *testing.T) {
	db := newTestDB(t)
	s := NewBroadcastStore(db)
	ctx := t.Context()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	gap := 5 * time.Second

	first, err := s.ClaimDispatchSlot(ctx, "default", now, gap)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !first.Equal(now) {
		t.Errorf("first slot = %v, want now", first)
	}
	second, err := s.ClaimDispatchSlot(ctx, "default", now, gap)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if !second.Equal(now.Add(gap)) {
		t.Errorf("second slot = %v, want now+gap", second)
	}
	cursor, found, err := s.DestinationCursor(ctx, "default")
	if err != nil || !found || !cursor.Equal(now.Add(2*gap)) {
		t.Errorf("cursor = %v, %v, %v; want now+2gap", cursor, found, err)
	}
	if _, found, err := s.DestinationCursor(ctx, "missing"); err != nil || found {
		t.Errorf("missing cursor = %v, %v; want not found", found, err)
	}
}

func TestBroadcastStore_CursorSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	dsn := dir + "/broadcast.db"
	open := func(t *testing.T) *sql.DB {
		t.Helper()
		db, err := OpenDB(t.Context(), dsn)
		if err != nil {
			t.Fatalf("OpenDB: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if err := Migrate(t.Context(), db); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
		return db
	}
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	first := NewBroadcastStore(open(t))
	if _, err := first.ClaimDispatchSlot(t.Context(), "default", now, 5*time.Second); err != nil {
		t.Fatalf("claim: %v", err)
	}
	second := NewBroadcastStore(open(t))
	cursor, found, err := second.DestinationCursor(t.Context(), "default")
	if err != nil || !found || !cursor.Equal(now.Add(5*time.Second)) {
		t.Errorf("reopened cursor = %v, %v, %v; want persisted now+gap", cursor, found, err)
	}
	slot, err := second.ClaimDispatchSlot(t.Context(), "default", now, 5*time.Second)
	if err != nil || !slot.Equal(now.Add(5*time.Second)) {
		t.Errorf("post-restart slot = %v, %v; want no burst", slot, err)
	}
}

func TestBroadcastStore_DeferExtendsCursor(t *testing.T) {
	db := newTestDB(t)
	s := NewBroadcastStore(db)
	ctx := t.Context()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

	if err := s.DeferDestination(ctx, "default", now.Add(time.Minute)); err != nil {
		t.Fatalf("defer: %v", err)
	}
	slot, err := s.ClaimDispatchSlot(ctx, "default", now, 5*time.Second)
	if err != nil || !slot.Equal(now.Add(time.Minute)) {
		t.Errorf("slot after defer = %v, %v; want deferred instant", slot, err)
	}
	if err := s.DeferDestination(ctx, "default", now); err != nil {
		t.Fatalf("shorter defer: %v", err)
	}
	cursor, _, err := s.DestinationCursor(ctx, "default")
	if err != nil || !cursor.Equal(now.Add(time.Minute).Add(5*time.Second)) {
		t.Errorf("cursor = %v, %v; want shorter defer ignored", cursor, err)
	}
	if err := s.DeferDestination(ctx, "", now); err == nil {
		t.Error("empty alias defer accepted")
	}
}

func TestBroadcastStore_DeferNoopReleasesTransaction(t *testing.T) {
	db := newTestDB(t)
	s := NewBroadcastStore(db)
	ctx := t.Context()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

	if err := s.DeferDestination(ctx, "default", now.Add(time.Minute)); err != nil {
		t.Fatalf("defer: %v", err)
	}
	for range 20 {
		if err := s.DeferDestination(ctx, "default", now); err != nil {
			t.Fatalf("noop defer: %v", err)
		}
	}
	slot, err := s.ClaimDispatchSlot(ctx, "default", now, 5*time.Second)
	if err != nil {
		t.Fatalf("write after noop defers: %v", err)
	}
	if !slot.Equal(now.Add(time.Minute)) {
		t.Errorf("slot after noop defers = %v, want deferred instant", slot)
	}
	if got := db.Stats().InUse; got != 0 {
		t.Errorf("open connections in use = %d, want 0 (leaked transaction)", got)
	}
}

func TestBroadcastStore_ConcurrentSlotsSerialized(t *testing.T) {
	db := newTestDB(t)
	s := NewBroadcastStore(db)
	ctx := t.Context()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

	const claims = 8
	slots := make([]time.Time, claims)
	var wg sync.WaitGroup
	for i := range claims {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slot, err := s.ClaimDispatchSlot(ctx, "burst", now, 5*time.Second)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			slots[i] = slot
		}()
	}
	wg.Wait()
	seen := map[time.Time]bool{}
	for _, slot := range slots {
		if slot.IsZero() {
			t.Fatal("claim recorded no slot")
		}
		if seen[slot] {
			t.Fatalf("duplicate slot %v: burst not paced", slot)
		}
		seen[slot] = true
	}
}

func TestBroadcastStore_ListHeldAndActive(t *testing.T) {
	db := newTestDB(t)
	s := NewBroadcastStore(db)
	ctx := t.Context()

	seed := func(id, state, alias string, notBefore time.Time) {
		t.Helper()
		item := testBroadcastItem(id, "cron", "key-"+id)
		item.State = state
		item.DestinationAlias = alias
		item.NotBefore = notBefore
		if _, _, err := s.InsertItem(ctx, item); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	morning := time.Date(2026, 9, 13, 7, 0, 0, 0, time.UTC)
	seed("held-1", BroadcastStateHeld, "default", morning)
	seed("held-2", BroadcastStateHeld, "default", morning.Add(time.Hour))
	seed("held-other", BroadcastStateHeld, "plain", morning)
	seed("sched-1", BroadcastStateScheduled, "default", morning)
	seed("done-1", BroadcastStateSucceeded, "default", morning)

	held, err := s.ListHeld(ctx, "default", BroadcastPriorityInfo, morning, 10)
	if err != nil {
		t.Fatalf("ListHeld(): %v", err)
	}
	if len(held) != 1 || held[0].ID != "held-1" {
		t.Errorf("held due = %v, want [held-1]", held)
	}
	all, err := s.ListHeld(ctx, "default", BroadcastPriorityInfo, morning.Add(2*time.Hour), 10)
	if err != nil || len(all) != 2 || all[0].ID != "held-1" || all[1].ID != "held-2" {
		t.Errorf("held window = %v, %v; want ordered held-1, held-2", all, err)
	}
	if _, err := s.ListHeld(ctx, "", BroadcastPriorityInfo, morning, 10); err == nil {
		t.Error("empty alias accepted")
	}
	if _, err := s.ListHeld(ctx, "default", BroadcastPriorityInfo, morning, 0); err == nil {
		t.Error("non-positive limit accepted")
	}

	active, err := s.ListActive(ctx, 10)
	if err != nil {
		t.Fatalf("ListActive(): %v", err)
	}
	if len(active) != 4 {
		t.Fatalf("active = %d, want 4 (done excluded)", len(active))
	}
	for _, item := range active {
		if item.State == BroadcastStateSucceeded {
			t.Errorf("terminal item listed: %+v", item)
		}
	}
}

func TestBroadcastStore_SettleAndAttempt(t *testing.T) {
	db := newTestDB(t)
	s := NewBroadcastStore(db)
	ctx := t.Context()
	now := time.Date(2026, 9, 13, 7, 0, 0, 0, time.UTC)

	if _, _, err := s.InsertItem(ctx, testBroadcastItem("bcst-1", "cron", "key-1")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO session (id, owner_id, metadata_json, created_at, updated_at) VALUES ('sess-1','cron','{}','2026-09-12T12:00:00Z','2026-09-12T12:00:00Z')`); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO effect_intent (id, session_id, turn_id, tool_call_id, idempotency_key, provider, operation, classification, state, request_digest, request_json, prepared_at, updated_at) VALUES ('effect-1','sess-1','','','key-1','discord','send_message','effectful','succeeded','digest','{}','2026-09-12T12:00:00Z','2026-09-12T12:00:00Z')`); err != nil {
		t.Fatalf("seed effect: %v", err)
	}
	if err := s.NoteAttempt(ctx, "bcst-1", 2, now); err != nil {
		t.Fatalf("NoteAttempt(): %v", err)
	}
	if err := s.Settle(ctx, "bcst-1", BroadcastStateSucceeded, "effect-1", now); err != nil {
		t.Fatalf("Settle(): %v", err)
	}
	item, err := s.Item(ctx, "bcst-1")
	if err != nil {
		t.Fatalf("Item(): %v", err)
	}
	if item.State != BroadcastStateSucceeded || item.EffectID != "effect-1" || item.AttemptCount != 2 {
		t.Errorf("settled item mismatch: %+v", item)
	}
	if err := s.Settle(ctx, "bcst-1", BroadcastStateFailed, "", now); err == nil {
		t.Error("double settle accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeBroadcastConflict {
		t.Errorf("double settle code = %v, %v; want broadcast_conflict", code, ok)
	}
	if err := s.Settle(ctx, "missing", BroadcastStateFailed, "", now); err == nil {
		t.Error("missing settle accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeBroadcastNotFound {
		t.Errorf("missing settle code = %v, %v; want broadcast_not_found", code, ok)
	}
	if err := s.Settle(ctx, "bcst-1", "flying", "", now); err == nil {
		t.Error("non-terminal settle accepted")
	}
	if err := s.NoteAttempt(ctx, "bcst-1", -1, now); err == nil {
		t.Error("negative attempt accepted")
	}
}

func TestBroadcastStore_SchemaVersion(t *testing.T) {
	db := newTestDB(t)
	applied, latest, err := SchemaVersions(t.Context(), db)
	if err != nil {
		t.Fatalf("SchemaVersions(): %v", err)
	}
	if latest != 11 {
		t.Errorf("latest schema = %d, want 11", latest)
	}
	if applied != 11 {
		t.Errorf("applied schema = %d, want 11", applied)
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
