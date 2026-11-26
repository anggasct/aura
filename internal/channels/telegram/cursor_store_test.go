package telegram

import (
	"context"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/store/storetest"
)

func TestTelegramCursorStore_AdvancesMonotonically(t *testing.T) {
	db := storetest.OpenMigrated(t)
	cursors, err := NewSQLiteCursorStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteCursorStore: %v", err)
	}
	ctx := t.Context()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

	if _, found, err := cursors.Load(ctx, "telegram:pocket"); err != nil || found {
		t.Fatalf("Load() on empty = found %v, err %v", found, err)
	}
	if err := cursors.Advance(ctx, "telegram:pocket", 101, now); err != nil {
		t.Fatalf("Advance(101): %v", err)
	}
	if err := cursors.Advance(ctx, "telegram:pocket", 90, now.Add(time.Second)); err != nil {
		t.Fatalf("Advance(90): %v", err)
	}
	if next, found, err := cursors.Load(ctx, "telegram:pocket"); err != nil || !found || next != 101 {
		t.Fatalf("Load() after a stale advance = %d %v %v, want 101", next, found, err)
	}
	if err := cursors.Advance(ctx, "telegram:pocket", 150, now); err != nil {
		t.Fatalf("Advance(150): %v", err)
	}
	if next, _, _ := cursors.Load(ctx, "telegram:pocket"); next != 150 {
		t.Fatalf("Load() = %d, want 150", next)
	}
	if _, found, _ := cursors.Load(ctx, "telegram:other"); found {
		t.Fatal("cursor leaked across sources")
	}
}

func TestTelegramCursorStore_RejectsInvalidInput(t *testing.T) {
	if _, err := NewSQLiteCursorStore(nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("NewSQLiteCursorStore(nil) = %v, want invalid_argument", err)
	}
	cursors, err := NewSQLiteCursorStore(storetest.OpenMigrated(t))
	if err != nil {
		t.Fatalf("NewSQLiteCursorStore: %v", err)
	}
	for name, call := range map[string]func() error{
		"empty source":   func() error { return cursors.Advance(t.Context(), "", 1, time.Now()) },
		"negative value": func() error { return cursors.Advance(t.Context(), "telegram:pocket", -1, time.Now()) },
	} {
		if code, ok := fault.CodeOf[ErrorCode](call()); !ok || code != ErrorCodeInvalidArgument {
			t.Errorf("%s: code = %q, want %q", name, code, ErrorCodeInvalidArgument)
		}
	}
}

func TestTelegramCursorStore_JoinsContextTransaction(t *testing.T) {
	db := storetest.OpenMigrated(t)
	cursors, err := NewSQLiteCursorStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteCursorStore: %v", err)
	}
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if err := sqlite.RunInTx(t.Context(), db, func(txCtx context.Context) error {
		if err := cursors.Advance(txCtx, "telegram:pocket", 42, now); err != nil {
			return err
		}
		next, found, err := cursors.Load(txCtx, "telegram:pocket")
		if err != nil || !found || next != 42 {
			t.Fatalf("Load inside tx = %d %v %v, want 42", next, found, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("RunInTx: %v", err)
	}
	if next, found, err := cursors.Load(t.Context(), "telegram:pocket"); err != nil || !found || next != 42 {
		t.Fatalf("Load after commit = %d %v %v, want 42", next, found, err)
	}
}
