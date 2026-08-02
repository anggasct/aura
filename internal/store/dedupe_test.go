package store

import (
	"context"
	"testing"
	"time"
)

func TestDedupeAcceptClaimsAndReplays(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	dedupe := NewDedupeStore(db)

	accepted := newEvent("session-1", 1)
	accepted.TurnID = "turn-1"
	accepted.Kind = "turn.accepted"

	turnID, created, err := dedupe.Accept(ctx, "telegram", "ext-1", time.Now().Add(time.Hour), &accepted)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if !created || turnID != "turn-1" {
		t.Fatalf("Accept = (%q, %v), want (turn-1, true)", turnID, created)
	}

	events, err := dedupe.ListTurnEvents(ctx, "turn-1")
	if err != nil {
		t.Fatalf("ListTurnEvents: %v", err)
	}
	if len(events) != 1 || events[0].TurnID != "turn-1" {
		t.Fatalf("events = %+v, want the accepted event", events)
	}

	// A duplicate claim returns the original turn and writes nothing.
	duplicate := newEvent("session-1", 2)
	duplicate.TurnID = "turn-2"
	duplicate.Kind = "turn.accepted"
	turnID, created, err = dedupe.Accept(ctx, "telegram", "ext-1", time.Now().Add(time.Hour), &duplicate)
	if err != nil {
		t.Fatalf("duplicate Accept: %v", err)
	}
	if created || turnID != "turn-1" {
		t.Fatalf("duplicate Accept = (%q, %v), want (turn-1, false)", turnID, created)
	}
	events, err = dedupe.ListTurnEvents(ctx, "turn-1")
	if err != nil {
		t.Fatalf("ListTurnEvents after duplicate: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("events after duplicate = %d, want 1 — a duplicate must not create a second sequence", len(events))
	}
}

func TestDedupeAcceptHonorsExpiry(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	dedupe := NewDedupeStore(db)

	first := newEvent("session-1", 1)
	first.TurnID = "turn-1"
	first.Kind = "turn.accepted"
	if _, created, err := dedupe.Accept(ctx, "telegram", "ext-1", time.Now().Add(-time.Second), &first); err != nil || !created {
		t.Fatalf("first Accept = (%v, %v)", created, err)
	}

	second := newEvent("session-1", 2)
	second.TurnID = "turn-2"
	second.Kind = "turn.accepted"
	turnID, created, err := dedupe.Accept(ctx, "telegram", "ext-1", time.Now().Add(time.Hour), &second)
	if err != nil {
		t.Fatalf("reclaim Accept: %v", err)
	}
	if !created || turnID != "turn-2" {
		t.Fatalf("reclaim Accept = (%q, %v), want (turn-2, true) after expiry", turnID, created)
	}
}

func TestDedupeAcceptRejectsBadKey(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	dedupe := NewDedupeStore(db)

	accepted := newEvent("session-1", 1)
	if _, _, err := dedupe.Accept(ctx, "", "ext-1", time.Now().Add(time.Hour), &accepted); err == nil {
		t.Fatal("empty source accepted")
	}
	if _, _, err := dedupe.Accept(ctx, "telegram", "", time.Now().Add(time.Hour), &accepted); err == nil {
		t.Fatal("empty external id accepted")
	}
	if _, _, err := dedupe.Accept(ctx, "telegram", "ext-1", time.Now().Add(time.Hour), nil); err == nil {
		t.Fatal("nil accepted event accepted")
	}
}
