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

func TestListTurnActivityGroupsBySession(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	mustCreateSession(t, db, "session-2")
	events := NewEventStore(db)
	since := time.Now().UTC().Add(-time.Minute)

	nth := 0
	appendKind := func(sessionID, turnID, kind string) {
		t.Helper()
		nth++
		event := newEvent(sessionID, 0)
		event.ID = sessionID + "-activity-" + string(rune('a'+nth))
		event.TurnID = turnID
		event.Kind = kind
		if _, err := events.AppendSequenced(ctx, sessionID, &event); err != nil {
			t.Fatalf("append %s/%s: %v", sessionID, kind, err)
		}
	}
	appendKind("session-1", "turn-1", "turn.accepted")
	appendKind("session-1", "turn-1", "turn.completed")
	appendKind("session-1", "turn-2", "turn.accepted")
	appendKind("session-2", "turn-9", "turn.accepted")

	activity, err := events.ListTurnActivity(ctx, since)
	if err != nil {
		t.Fatalf("ListTurnActivity: %v", err)
	}
	if len(activity) != 2 {
		t.Fatalf("sessions = %d, want 2", len(activity))
	}
	byTurn := map[string][]string{}
	for _, turn := range activity["session-1"] {
		byTurn[turn.TurnID] = turn.Kinds
	}
	if len(byTurn["turn-1"]) != 2 || len(byTurn["turn-2"]) != 1 {
		t.Fatalf("session-1 activity = %v, want turn-1 x2 kinds and turn-2 x1", byTurn)
	}
	if _, err := events.ListTurnActivity(ctx, time.Time{}); err == nil {
		t.Fatal("expected an error for a zero start time")
	}
}
