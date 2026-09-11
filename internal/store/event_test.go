package store

import (
	"context"
	"testing"
	"time"
)

func TestAppendSequencedAssignsContiguousSequences(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	events := NewEventStore(db)

	first := newEvent("session-1", 0)
	first.ID = "session-1-first"
	seq, err := events.AppendSequenced(ctx, "session-1", &first)
	if err != nil {
		t.Fatalf("append first: %v", err)
	}
	if seq != 1 || first.Sequence != 1 {
		t.Fatalf("first sequence = %d (event %d), want 1", seq, first.Sequence)
	}
	second := newEvent("session-1", 0)
	second.ID = "session-1-second"
	seq, err = events.AppendSequenced(ctx, "session-1", &second)
	if err != nil {
		t.Fatalf("append second: %v", err)
	}
	if seq != 2 {
		t.Fatalf("second sequence = %d, want 2", seq)
	}
	preset := newEvent("session-1", 99)
	preset.ID = "session-1-preset"
	seq, err = events.AppendSequenced(ctx, "session-1", &preset)
	if err != nil {
		t.Fatalf("append preset: %v", err)
	}
	if seq != 3 || preset.Sequence != 3 {
		t.Fatalf("preset reassigned = %d, want 3", seq)
	}
}

func TestAcceptAssignsSequenceWhenZero(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	dedupe := NewDedupeStore(db)

	accepted := newEvent("session-1", 0)
	accepted.TurnID = "turn-1"
	accepted.Kind = "turn.accepted"
	turnID, created, err := dedupe.Accept(ctx, "terminal", "ext-1", time.Now().Add(time.Hour), &accepted)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if !created || turnID != "turn-1" {
		t.Fatalf("created = %v turn = %q, want true turn-1", created, turnID)
	}
	if accepted.Sequence != 1 {
		t.Fatalf("assigned sequence = %d, want 1", accepted.Sequence)
	}
}
