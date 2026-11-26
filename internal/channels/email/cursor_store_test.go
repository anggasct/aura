package email

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/store/storetest"
)

func mustCursorStore(t *testing.T) *SQLiteCursorStore {
	t.Helper()
	store, err := NewSQLiteCursorStore(storetest.OpenMigrated(t))
	if err != nil {
		t.Fatalf("NewSQLiteCursorStore: %v", err)
	}
	return store
}

func TestEmailCursorStore_RejectsNilDB(t *testing.T) {
	store, err := NewSQLiteCursorStore(nil)
	if store != nil || !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("NewSQLiteCursorStore(nil) = %v, %v; want invalid_argument", store, err)
	}
}

func TestEmailCursorStore_AdvancesMonotonicallyWithinOneValidity(t *testing.T) {
	cursors := mustCursorStore(t)
	ctx := t.Context()
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	const source = "email:work"

	if _, found, err := cursors.Load(ctx, source); err != nil || found {
		t.Fatalf("Load() on empty = found %v, err %v", found, err)
	}
	if err := cursors.Save(ctx, source, Cursor{UIDValidity: 7, LastUID: 40}, now); err != nil {
		t.Fatalf("Save(40): %v", err)
	}
	if err := cursors.Save(ctx, source, Cursor{UIDValidity: 7, LastUID: 12}, now.Add(time.Second)); err != nil {
		t.Fatalf("Save(12): %v", err)
	}
	got, found, err := cursors.Load(ctx, source)
	if err != nil || !found || got != (Cursor{UIDValidity: 7, LastUID: 40}) {
		t.Fatalf("Load() after a stale save = %+v %v %v, want validity 7 uid 40", got, found, err)
	}
	if err := cursors.Save(ctx, source, Cursor{UIDValidity: 7, LastUID: 55}, now); err != nil {
		t.Fatalf("Save(55): %v", err)
	}
	if got, _, _ := cursors.Load(ctx, source); got.LastUID != 55 {
		t.Fatalf("LastUID = %d, want 55", got.LastUID)
	}
	if _, found, _ := cursors.Load(ctx, "email:other"); found {
		t.Fatal("cursor leaked across sources")
	}
}

func TestEmailCursorStore_ValidityChangeReplacesTheCursor(t *testing.T) {
	cursors := mustCursorStore(t)
	ctx := t.Context()
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	const source = "email:work"

	if err := cursors.Save(ctx, source, Cursor{UIDValidity: 7, LastUID: 900}, now); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := cursors.Save(ctx, source, Cursor{UIDValidity: 8, LastUID: 3}, now.Add(time.Second)); err != nil {
		t.Fatalf("Save(new validity): %v", err)
	}
	got, found, err := cursors.Load(ctx, source)
	if err != nil || !found || got != (Cursor{UIDValidity: 8, LastUID: 3}) {
		t.Fatalf("Load() = %+v %v %v, want validity 8 uid 3", got, found, err)
	}
}

func TestEmailCursorStore_RejectsEmptySource(t *testing.T) {
	cursors := mustCursorStore(t)
	err := cursors.Save(t.Context(), "", Cursor{UIDValidity: 1, LastUID: 1}, time.Now())
	if !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("err = %v, want %q", err, ErrorCodeInvalidArgument)
	}
}

func TestEmailCursorStore_RejectsOutOfRangeValues(t *testing.T) {
	cases := map[string]struct {
		validity int64
		lastUID  int64
	}{
		"validity above uint32": {validity: int64(math.MaxUint32) + 1, lastUID: 1},
		"last_uid above uint32": {validity: 1, lastUID: int64(math.MaxUint32) + 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			db := storetest.OpenMigrated(t)
			cursors, err := NewSQLiteCursorStore(db)
			if err != nil {
				t.Fatalf("NewSQLiteCursorStore: %v", err)
			}
			_, err = db.ExecContext(t.Context(), `INSERT INTO email_cursor (source, uid_validity, last_uid, updated_at) VALUES (?, ?, ?, ?)`, "email:work", tc.validity, tc.lastUID, sqlite.FormatTime(time.Now().UTC()))
			if err != nil {
				t.Fatalf("insert corrupt cursor: %v", err)
			}
			if _, _, err := cursors.Load(t.Context(), "email:work"); !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("Load() error = %v, want %q", err, ErrorCodeInvalidArgument)
			}
		})
	}
}

func TestEmailCursorStore_SharesContextTransaction(t *testing.T) {
	db := storetest.OpenMigrated(t)
	cursors, err := NewSQLiteCursorStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteCursorStore: %v", err)
	}
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	rollbackErr := fault.New(ErrorCodeIntakeFailed, "rollback")
	err = sqlite.RunInTx(t.Context(), db, func(ctx context.Context) error {
		if err := cursors.Save(ctx, "email:work", Cursor{UIDValidity: 9, LastUID: 10}, now); err != nil {
			return err
		}
		got, found, err := cursors.Load(ctx, "email:work")
		if err != nil || !found || got != (Cursor{UIDValidity: 9, LastUID: 10}) {
			t.Fatalf("in-tx Load() = %+v %v %v", got, found, err)
		}
		return rollbackErr
	})
	if !fault.HasCode(err, ErrorCodeIntakeFailed) {
		t.Fatalf("RunInTx() = %v, want rollback error", err)
	}
	if _, found, err := cursors.Load(t.Context(), "email:work"); err != nil || found {
		t.Fatalf("after rollback Load() = found %v, err %v; want rolled back", found, err)
	}
}
