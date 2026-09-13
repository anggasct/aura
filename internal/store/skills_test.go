package store

import (
	"context"
	"sync"
	"testing"
)

func testSkillRow(id string) *SkillRow {
	return &SkillRow{
		ID:         id,
		Name:       "pdf-tools",
		Origin:     `{"scope":"local"}`,
		Digest:     "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		State:      SkillStateQuarantined,
		Validation: `[]`,
		Requested:  `["read"]`,
		Granted:    `[]`,
	}
}

func TestSkillUpsertDedupesDigest(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	store := NewSkillStore(db)

	inserted, err := store.UpsertScan(ctx, testSkillRow("local/pdf-tools@0123456789ab"))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !inserted {
		t.Errorf("first upsert should report inserted")
	}
	inserted, err = store.UpsertScan(ctx, testSkillRow("local/pdf-tools@0123456789ab"))
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if inserted {
		t.Errorf("same digest upsert should report dedupe success")
	}
	got, err := store.GetSkill(ctx, "local/pdf-tools@0123456789ab")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "pdf-tools" || got.State != SkillStateQuarantined {
		t.Errorf("row = %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("timestamps not set: %+v", got)
	}
	if got.ReviewedAt != nil {
		t.Errorf("reviewed_at should be null: %+v", got)
	}
}

func TestSkillUpsertNewDigestVersions(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	store := NewSkillStore(db)

	first := testSkillRow("local/pdf-tools@aaaaaaaaaaaaaaaa")
	second := testSkillRow("local/pdf-tools@bbbbbbbbbbbbbbbb")
	second.Digest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for _, row := range []*SkillRow{first, second} {
		if _, err := store.UpsertScan(ctx, row); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	rows, err := store.ListSkillsByState(ctx, SkillStateQuarantined, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("rows = %d, want 2 versions", len(rows))
	}
}

func TestSkillGetNotFound(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	_, err := NewSkillStore(db).GetSkill(ctx, "local/missing@0123456789ab")
	wantCode(t, err, ErrorCodeSkillNotFound)
}

func TestSkillStoreInvalid(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	store := NewSkillStore(db)

	var nilCtx context.Context
	if _, err := store.UpsertScan(nilCtx, testSkillRow("x")); err == nil {
		t.Errorf("nil ctx should fail")
	}
	if _, err := store.UpsertScan(ctx, nil); err == nil {
		t.Errorf("nil row should fail")
	}
	bad := testSkillRow("x")
	bad.State = "flying"
	wantCode(t, mustUpsertErr(store.UpsertScan(ctx, bad)), ErrorCodeSkillInvalid)
	bad = testSkillRow("x")
	bad.Requested = "{broken"
	wantCode(t, mustUpsertErr(store.UpsertScan(ctx, bad)), ErrorCodeSkillInvalid)
	bad = testSkillRow("")
	wantCode(t, mustUpsertErr(store.UpsertScan(ctx, bad)), ErrorCodeSkillInvalid)
	if _, err := store.GetSkill(ctx, ""); err == nil {
		t.Errorf("empty id should fail")
	}
	if _, err := store.ListSkillsByState(ctx, "flying", 0); err == nil {
		t.Errorf("bad state should fail")
	}
	if _, err := store.ListSkillsByState(ctx, SkillStateActive, -1); err == nil {
		t.Errorf("negative limit should fail")
	}
	if _, err := store.ListSkillsByState(nilCtx, SkillStateActive, 0); err == nil {
		t.Errorf("nil ctx should fail")
	}
}

func mustUpsertErr(_ bool, err error) error {
	return err
}

func TestSkillConcurrentUpsertSameDigest(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	store := NewSkillStore(db)

	const workers = 8
	inserted := make([]bool, workers)
	errs := make([]error, workers)
	var group sync.WaitGroup
	for index := range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			done, err := store.UpsertScan(ctx, testSkillRow("local/race@0123456789ab"))
			inserted[index] = done
			errs[index] = err
		}()
	}
	group.Wait()
	wins := 0
	for index := range workers {
		if errs[index] != nil {
			t.Fatalf("worker %d: %v", index, errs[index])
		}
		if inserted[index] {
			wins++
		}
	}
	if wins != 1 {
		t.Errorf("inserted winners = %d, want exactly 1", wins)
	}
}
