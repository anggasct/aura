package store

import (
	"testing"
	"time"
)

func TestEmailCursorStore_AdvancesMonotonicallyWithinOneValidity(t *testing.T) {
	cursors := NewEmailCursorStore(newTestDB(t))
	ctx := t.Context()
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	const source = "email:work"

	if _, found, err := cursors.Load(ctx, source); err != nil || found {
		t.Fatalf("Load() on empty = found %v, err %v", found, err)
	}
	if err := cursors.Save(ctx, source, EmailCursor{UIDValidity: 7, LastUID: 40}, now); err != nil {
		t.Fatalf("Save(40): %v", err)
	}
	if err := cursors.Save(ctx, source, EmailCursor{UIDValidity: 7, LastUID: 12}, now.Add(time.Second)); err != nil {
		t.Fatalf("Save(12): %v", err)
	}
	got, found, err := cursors.Load(ctx, source)
	if err != nil || !found || got != (EmailCursor{UIDValidity: 7, LastUID: 40}) {
		t.Fatalf("Load() after a stale save = %+v %v %v, want validity 7 uid 40", got, found, err)
	}
	if err := cursors.Save(ctx, source, EmailCursor{UIDValidity: 7, LastUID: 55}, now); err != nil {
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
	cursors := NewEmailCursorStore(newTestDB(t))
	ctx := t.Context()
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	const source = "email:work"

	if err := cursors.Save(ctx, source, EmailCursor{UIDValidity: 7, LastUID: 900}, now); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := cursors.Save(ctx, source, EmailCursor{UIDValidity: 8, LastUID: 3}, now.Add(time.Second)); err != nil {
		t.Fatalf("Save(new validity): %v", err)
	}
	got, found, err := cursors.Load(ctx, source)
	if err != nil || !found || got != (EmailCursor{UIDValidity: 8, LastUID: 3}) {
		t.Fatalf("Load() = %+v %v %v, want validity 8 uid 3", got, found, err)
	}
}

func TestEmailCursorStore_RejectsEmptySource(t *testing.T) {
	cursors := NewEmailCursorStore(newTestDB(t))
	err := cursors.Save(t.Context(), "", EmailCursor{UIDValidity: 1, LastUID: 1}, time.Now())
	if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
		t.Fatalf("code = %q, want %q", code, ErrorCodeInvalidArgument)
	}
}
