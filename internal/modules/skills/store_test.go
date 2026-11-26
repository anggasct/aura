package skills

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/store/storetest"
)

func testSkillRecord(id string) *Record {
	return &Record{
		ID:         id,
		Name:       "pdf-tools",
		Origin:     `{"scope":"local"}`,
		Digest:     "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		State:      StateQuarantined,
		Validation: `[]`,
		Requested:  `["read"]`,
		Granted:    `[]`,
	}
}

func mustNewSQLiteStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(storetest.OpenMigrated(t))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	return store
}

func wantSkillCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("err = nil, want %s", want)
	}
	if !fault.HasCode(err, want) {
		t.Fatalf("err = %v, want %s", err, want)
	}
}

func TestNewSQLiteStoreRejectsNilDB(t *testing.T) {
	if _, err := NewSQLiteStore(nil); err == nil {
		t.Fatal("NewSQLiteStore(nil) = nil, want error")
	} else {
		wantSkillCode(t, err, ErrorCodeInvalidArgument)
	}
}

func TestSQLiteStoreJoinsCallersTransaction(t *testing.T) {
	ctx := t.Context()
	db := storetest.OpenMigrated(t)
	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	rollbackErr := errors.New("abort transaction")
	record := testSkillRecord("local/tx@0123456789ab")
	err = sqlite.RunInTx(ctx, db, func(txCtx context.Context) error {
		inserted, upsertErr := store.UpsertScan(txCtx, record)
		if upsertErr != nil {
			t.Fatalf("upsert in tx: %v", upsertErr)
		}
		if !inserted {
			t.Fatalf("upsert in tx reported not inserted")
		}
		got, getErr := store.Get(txCtx, record.ID)
		if getErr != nil {
			t.Fatalf("get in tx: %v", getErr)
		}
		if got.ID != record.ID {
			t.Fatalf("get in tx = %+v", got)
		}
		return rollbackErr
	})
	if !errors.Is(err, rollbackErr) {
		t.Fatalf("RunInTx err = %v, want %v", err, rollbackErr)
	}
	if _, err := store.Get(ctx, record.ID); err == nil {
		t.Fatal("rolled-back skill remained visible outside transaction")
	} else {
		wantSkillCode(t, err, ErrorCodeSkillNotFound)
	}
}

func TestSkillUpsertDedupesDigest(t *testing.T) {
	ctx := t.Context()
	store := mustNewSQLiteStore(t)

	inserted, err := store.UpsertScan(ctx, testSkillRecord("local/pdf-tools@0123456789ab"))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !inserted {
		t.Errorf("first upsert should report inserted")
	}
	inserted, err = store.UpsertScan(ctx, testSkillRecord("local/pdf-tools@0123456789ab"))
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if inserted {
		t.Errorf("same digest upsert should report dedupe success")
	}
	got, err := store.Get(ctx, "local/pdf-tools@0123456789ab")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "pdf-tools" || got.State != StateQuarantined {
		t.Errorf("record = %+v", got)
	}
	if got.ReviewedAt != "" {
		t.Errorf("reviewed_at should be empty: %+v", got)
	}
}

func TestSkillUpsertNewDigestVersions(t *testing.T) {
	ctx := t.Context()
	store := mustNewSQLiteStore(t)

	first := testSkillRecord("local/pdf-tools@aaaaaaaaaaaaaaaa")
	second := testSkillRecord("local/pdf-tools@bbbbbbbbbbbbbbbb")
	second.Digest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for _, record := range []*Record{first, second} {
		if _, err := store.UpsertScan(ctx, record); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	rows, err := store.ListByState(ctx, StateQuarantined, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("rows = %d, want 2 versions", len(rows))
	}
	all, err := store.ListAll(ctx, 0)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("all = %d, want 2 versions", len(all))
	}
}

func TestSkillGetNotFound(t *testing.T) {
	ctx := t.Context()
	_, err := mustNewSQLiteStore(t).Get(ctx, "local/missing@0123456789ab")
	wantSkillCode(t, err, ErrorCodeSkillNotFound)
}

func TestSkillStoreInvalid(t *testing.T) {
	ctx := t.Context()
	store := mustNewSQLiteStore(t)

	var nilCtx context.Context
	if _, err := store.UpsertScan(nilCtx, testSkillRecord("x")); err == nil {
		t.Errorf("nil ctx should fail")
	}
	if _, err := store.UpsertScan(ctx, nil); err == nil {
		t.Errorf("nil record should fail")
	}
	bad := testSkillRecord("x")
	bad.State = "flying"
	wantSkillCode(t, mustUpsertErr(store.UpsertScan(ctx, bad)), ErrorCodeSkillInvalid)
	bad = testSkillRecord("x")
	bad.Requested = "{broken"
	wantSkillCode(t, mustUpsertErr(store.UpsertScan(ctx, bad)), ErrorCodeSkillInvalid)
	bad = testSkillRecord("")
	wantSkillCode(t, mustUpsertErr(store.UpsertScan(ctx, bad)), ErrorCodeSkillInvalid)
	if _, err := store.Get(ctx, ""); err == nil {
		t.Errorf("empty id should fail")
	}
	if _, err := store.ListByState(ctx, "flying", 0); err == nil {
		t.Errorf("bad state should fail")
	}
	if _, err := store.ListByState(ctx, StateActive, -1); err == nil {
		t.Errorf("negative limit should fail")
	}
	if _, err := store.ListByState(nilCtx, StateActive, 0); err == nil {
		t.Errorf("nil ctx should fail")
	}
	if _, err := store.ListAll(ctx, -1); err == nil {
		t.Errorf("negative limit should fail")
	}
}

func mustUpsertErr(_ bool, err error) error {
	return err
}

func TestSkillConcurrentUpsertSameDigest(t *testing.T) {
	ctx := t.Context()
	store := mustNewSQLiteStore(t)

	const workers = 8
	inserted := make([]bool, workers)
	errs := make([]error, workers)
	var group sync.WaitGroup
	for index := range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			done, err := store.UpsertScan(ctx, testSkillRecord("local/race@0123456789ab"))
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

func TestSkillAcceptBindsDigestAndGrants(t *testing.T) {
	ctx := t.Context()
	store := mustNewSQLiteStore(t)

	record := testSkillRecord("local/pdf@0123456789ab")
	if _, err := store.UpsertScan(ctx, record); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.AcceptSkill(ctx, record.ID, record.Digest, `["read"]`); err != nil {
		t.Fatalf("accept: %v", err)
	}
	got, err := store.Get(ctx, record.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != StateActive || got.Granted != `["read"]` {
		t.Errorf("record = %+v", got)
	}
	if got.ReviewedAt == "" {
		t.Errorf("reviewed_at not set")
	}
	if err := store.AcceptSkill(ctx, record.ID, record.Digest, `[]`); err == nil {
		t.Errorf("second accept should fail")
	} else {
		wantSkillCode(t, err, ErrorCodeSkillInvalid)
	}
	if err := store.DisableSkill(ctx, record.ID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	disabled, err := store.Get(ctx, record.ID)
	if err != nil {
		t.Fatalf("get disabled: %v", err)
	}
	if disabled.State != StateDisabled {
		t.Errorf("state = %s, want %s", disabled.State, StateDisabled)
	}
}

func TestSkillAcceptRejectsChangedDigest(t *testing.T) {
	ctx := t.Context()
	store := mustNewSQLiteStore(t)

	record := testSkillRecord("local/pdf@0123456789ab")
	if _, err := store.UpsertScan(ctx, record); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	err := store.AcceptSkill(ctx, record.ID, "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", `[]`)
	wantSkillCode(t, err, ErrorCodeSkillDigestChanged)
	if err := store.RejectSkill(ctx, "local/missing@0123456789ab"); err == nil {
		t.Errorf("missing reject should fail")
	} else {
		wantSkillCode(t, err, ErrorCodeSkillNotFound)
	}
}

func TestSkillRejectTransitions(t *testing.T) {
	ctx := t.Context()
	store := mustNewSQLiteStore(t)

	record := testSkillRecord("local/pdf@0123456789ab")
	if _, err := store.UpsertScan(ctx, record); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.RejectSkill(ctx, record.ID); err != nil {
		t.Fatalf("reject: %v", err)
	}
	got, err := store.Get(ctx, record.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != StateRejected || got.ReviewedAt == "" {
		t.Errorf("record = %+v", got)
	}
	if err := store.RejectSkill(ctx, record.ID); err == nil {
		t.Errorf("second reject should fail")
	} else {
		wantSkillCode(t, err, ErrorCodeSkillInvalid)
	}
	if err := store.AcceptSkill(nilCtxForStore(), record.ID, record.Digest, `[]`); err == nil {
		t.Errorf("nil ctx should fail")
	}
}

func nilCtxForStore() context.Context {
	var ctx context.Context
	return ctx
}
