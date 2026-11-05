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

func TestDedupeClaimedReportsLiveKeysOnly(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	dedupe := NewDedupeStore(db)

	claimed, err := dedupe.Claimed(ctx, "telegram", "ext-1")
	if err != nil || claimed {
		t.Fatalf("Claimed before accept = (%v, %v), want (false, nil)", claimed, err)
	}
	live := newEvent("session-1", 1)
	live.TurnID = "turn-1"
	live.Kind = "turn.accepted"
	if _, _, err := dedupe.Accept(ctx, "telegram", "ext-1", time.Now().Add(time.Hour), &live); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	expired := newEvent("session-1", 2)
	expired.TurnID = "turn-2"
	expired.Kind = "turn.accepted"
	if _, _, err := dedupe.Accept(ctx, "telegram", "ext-2", time.Now().Add(-time.Hour), &expired); err != nil {
		t.Fatalf("Accept expired: %v", err)
	}
	cases := []struct {
		name       string
		source     string
		externalID string
		want       bool
	}{
		{name: "live key", source: "telegram", externalID: "ext-1", want: true},
		{name: "expired key", source: "telegram", externalID: "ext-2", want: false},
		{name: "other source", source: "discord", externalID: "ext-1", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := dedupe.Claimed(ctx, tc.source, tc.externalID)
			if err != nil || got != tc.want {
				t.Errorf("Claimed = (%v, %v), want (%v, nil)", got, err, tc.want)
			}
		})
	}
	if _, err := dedupe.Claimed(ctx, "", "ext-1"); err == nil {
		t.Error("Claimed with an empty source succeeded")
	}
}
