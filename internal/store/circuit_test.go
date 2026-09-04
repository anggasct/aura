package store

import (
	"context"
	"testing"
	"time"
)

func TestCircuitCheckpointStore_SaveLoadDelete(t *testing.T) {
	db := newTestDB(t)
	s := NewCircuitCheckpointStore(db)
	ctx := context.Background()

	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	openUntil := now.Add(5 * time.Minute)

	cp1 := CircuitCheckpoint{
		CircuitKey:          "primary|https://api.openai.com",
		ConfigDigest:        "abc12345",
		State:               "open",
		ConsecutiveFailures: 3,
		OpenUntil:           &openUntil,
		UpdatedAt:           now,
	}
	if err := s.Save(ctx, &cp1); err != nil {
		t.Fatalf("Save(cp1): %v", err)
	}

	// Load and verify
	loaded, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("len(loaded) = %d, want 1", len(loaded))
	}
	got := loaded[0]
	if got.CircuitKey != cp1.CircuitKey || got.ConfigDigest != cp1.ConfigDigest || got.State != cp1.State || got.ConsecutiveFailures != 3 {
		t.Errorf("loaded checkpoint mismatch: %+v, want %+v", got, cp1)
	}
	if got.OpenUntil == nil || !got.OpenUntil.Equal(openUntil) {
		t.Errorf("open_until = %v, want %v", got.OpenUntil, openUntil)
	}

	// Update (upsert)
	cp1.ConsecutiveFailures = 4
	cp1.State = "half_open"
	if err := s.Save(ctx, &cp1); err != nil {
		t.Fatalf("Save updated cp1: %v", err)
	}

	loaded, err = s.Load(ctx)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("Load() after update: %v, len = %d", err, len(loaded))
	}
	if loaded[0].ConsecutiveFailures != 4 || loaded[0].State != "half_open" {
		t.Errorf("updated checkpoint mismatch: %+v", loaded[0])
	}

	// Delete
	if err := s.Delete(ctx, cp1.CircuitKey); err != nil {
		t.Fatalf("Delete(): %v", err)
	}
	loaded, err = s.Load(ctx)
	if err != nil {
		t.Fatalf("Load() after delete: %v", err)
	}
	if len(loaded) != 0 {
		t.Errorf("len(loaded) = %d after delete, want 0", len(loaded))
	}
}
