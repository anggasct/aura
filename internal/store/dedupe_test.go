package store

import (
	"context"
	"database/sql"
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

func claimInTx(t *testing.T, db *sql.DB, claim *DedupeClaim) (string, bool) {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	turnID, created, err := ClaimDedupeTx(t.Context(), tx, claim)
	if err != nil {
		t.Fatalf("ClaimDedupeTx(%s, %s): %v", claim.ExternalID, claim.TurnID, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return turnID, created
}

func TestClaimDedupeTxClaimsOncePerWindow(t *testing.T) {
	db := newTestDB(t)
	start := time.Date(2030, 1, 2, 3, 4, 0, 0, time.UTC)
	window := &DedupeClaim{Source: "telegram", ExternalID: "ext-1", TurnID: "turn-1", Now: start, ExpiresAt: start.Add(time.Hour)}
	if turnID, created := claimInTx(t, db, window); !created || turnID != "turn-1" {
		t.Fatalf("first claim = (%q, %v), want (turn-1, true)", turnID, created)
	}
	if turnID, created := claimInTx(t, db, &DedupeClaim{Source: "telegram", ExternalID: "ext-1", TurnID: "turn-2", Now: start.Add(time.Minute), ExpiresAt: start.Add(2 * time.Hour)}); created || turnID != "turn-1" {
		t.Fatalf("second claim = (%q, %v), want (turn-1, false)", turnID, created)
	}
	later := start.Add(2 * time.Hour)
	if turnID, created := claimInTx(t, db, &DedupeClaim{Source: "telegram", ExternalID: "ext-1", TurnID: "turn-3", Now: later, ExpiresAt: later.Add(time.Hour)}); !created || turnID != "turn-3" {
		t.Fatalf("claim after expiry = (%q, %v), want (turn-3, true)", turnID, created)
	}
	var storedTurn, acceptedAt string
	if err := db.QueryRowContext(t.Context(), `SELECT turn_id, accepted_at FROM ingress_dedupe WHERE source = 'telegram' AND external_id = 'ext-1'`).Scan(&storedTurn, &acceptedAt); err != nil {
		t.Fatalf("read claim: %v", err)
	}
	if storedTurn != "turn-3" || acceptedAt != later.Format(time.RFC3339Nano) {
		t.Fatalf("stored claim = (%q, %q), want (turn-3, %q)", storedTurn, acceptedAt, later.Format(time.RFC3339Nano))
	}

	second := time.Date(2030, 1, 2, 5, 0, 0, 0, time.UTC)
	claimInTx(t, db, &DedupeClaim{Source: "telegram", ExternalID: "ext-2", TurnID: "turn-a", Now: second.Add(-time.Hour), ExpiresAt: second.Add(100 * time.Millisecond)})
	if turnID, created := claimInTx(t, db, &DedupeClaim{Source: "telegram", ExternalID: "ext-2", TurnID: "turn-b", Now: second, ExpiresAt: second.Add(time.Hour)}); created || turnID != "turn-a" {
		t.Fatalf("claim at the whole second before a fractional expiry = (%q, %v), want (turn-a, false)", turnID, created)
	}
	if turnID, created := claimInTx(t, db, &DedupeClaim{Source: "telegram", ExternalID: "ext-2", TurnID: "turn-c", Now: second.Add(200 * time.Millisecond), ExpiresAt: second.Add(time.Hour)}); !created || turnID != "turn-c" {
		t.Fatalf("claim after a fractional expiry = (%q, %v), want (turn-c, true)", turnID, created)
	}

	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, created, err := ClaimDedupeTx(t.Context(), tx, &DedupeClaim{Source: "telegram", ExternalID: "ext-3", TurnID: "turn-x", Now: start, ExpiresAt: later}); err != nil || !created {
		t.Fatalf("claim in a rolled back transaction = (%v, %v)", created, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	var kept int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM ingress_dedupe WHERE external_id = 'ext-3'`).Scan(&kept); err != nil || kept != 0 {
		t.Fatalf("rows after rollback = %d (%v), want 0", kept, err)
	}

	valid := DedupeClaim{Source: "telegram", ExternalID: "ext-4", TurnID: "turn-4", Now: start, ExpiresAt: later}
	invalid := []struct {
		name  string
		nilTx bool
		claim *DedupeClaim
	}{
		{name: "nil transaction", nilTx: true, claim: &valid},
		{name: "nil claim"},
		{name: "empty source", claim: &DedupeClaim{ExternalID: "ext-4", TurnID: "turn-4", Now: start, ExpiresAt: later}},
		{name: "empty external id", claim: &DedupeClaim{Source: "telegram", TurnID: "turn-4", Now: start, ExpiresAt: later}},
		{name: "zero now", claim: &DedupeClaim{Source: "telegram", ExternalID: "ext-4", TurnID: "turn-4", ExpiresAt: later}},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback() }()
			target := tx
			if tc.nilTx {
				target = nil
			}
			_, _, err = ClaimDedupeTx(t.Context(), target, tc.claim)
			if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
				t.Fatalf("err = %v, want %s", err, ErrorCodeInvalidArgument)
			}
		})
	}
}
