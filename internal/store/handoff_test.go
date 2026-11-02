package store

import (
	"testing"
	"time"
)

func TestHandoffOpenCheckReleaseAndTimeout(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	seedPersonaSession(t, db, "lobby/discord:owner/dm:601", "discord:601", "lobby")
	handoffs := NewHandoffStore(db)
	opened := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	if state, _, err := handoffs.Check(ctx, "lobby/discord:owner/dm:601", time.Hour, opened); err != nil || state != HandoffNone {
		t.Fatalf("Check before open = %q, %v", state, err)
	}
	open := &Handoff{SessionID: "lobby/discord:owner/dm:601", Reason: "refund request", PrincipalID: "discord:601", AgentID: "lobby", Account: "owner", ReplyChannelID: "4400", OpenedAt: opened}
	if err := handoffs.Open(ctx, open); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if state, got, err := handoffs.Check(ctx, open.SessionID, time.Hour, opened.Add(59*time.Minute)); err != nil || state != HandoffOpen || got.ReplyChannelID != "4400" || got.Account != "owner" {
		t.Fatalf("Check inside timeout = %q, %+v, %v", state, got, err)
	}
	if list, err := handoffs.ListOpen(ctx); err != nil || len(list) != 1 || list[0].Reason != "refund request" {
		t.Fatalf("ListOpen = %+v, %v", list, err)
	}
	if state, got, err := handoffs.Check(ctx, open.SessionID, time.Hour, opened.Add(time.Hour)); err != nil || state != HandoffExpired || got.ReleasedAt.IsZero() {
		t.Fatalf("Check at timeout = %q, %+v, %v; want auto-release", state, got, err)
	}
	if state, _, _ := handoffs.Check(ctx, open.SessionID, time.Hour, opened.Add(2*time.Hour)); state != HandoffNone {
		t.Fatalf("Check after auto-release = %q, want none", state)
	}
	open.OpenedAt = opened.Add(3 * time.Hour)
	if err := handoffs.Open(ctx, open); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	released, ok, err := handoffs.Release(ctx, open.SessionID, opened.Add(4*time.Hour))
	if err != nil || !ok || !released.ReleasedAt.Equal(opened.Add(4*time.Hour)) {
		t.Fatalf("Release = %+v, %v, %v", released, ok, err)
	}
	if _, ok, _ := handoffs.Release(ctx, open.SessionID, opened.Add(5*time.Hour)); ok {
		t.Fatal("released twice")
	}
	if list, _ := handoffs.ListOpen(ctx); len(list) != 0 {
		t.Fatalf("ListOpen after release = %+v", list)
	}
	if err := handoffs.Open(ctx, &Handoff{SessionID: open.SessionID}); err == nil {
		t.Fatal("handoff without a reason accepted")
	}
}
