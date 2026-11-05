package store

import (
	"path/filepath"
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
	open := &Handoff{SessionID: "lobby/discord:owner/dm:601", Reason: "refund request", PrincipalID: "discord:601", AgentID: "lobby", Account: "owner", Channel: HandoffChannelDiscord, ReplyChannelID: "4400", OpenedAt: opened}
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
	if err := handoffs.Open(ctx, &Handoff{SessionID: open.SessionID, Channel: HandoffChannelDiscord}); err == nil {
		t.Fatal("handoff without a reason accepted")
	}
}

func TestHandoffRoundTripsATelegramReplyTarget(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	sessionID := "lobby/telegram:pocket/chat:555001"
	seedPersonaSession(t, db, sessionID, "telegram:555001", "lobby")
	handoffs := NewHandoffStore(db)
	opened := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	open := &Handoff{
		SessionID: sessionID, Reason: "refund request", PrincipalID: "telegram:555001", AgentID: "lobby",
		Account: "pocket", Channel: HandoffChannelTelegram, ReplyChannelID: "-1001234567890", ReplyThreadID: "77", OpenedAt: opened,
	}
	if err := handoffs.Open(ctx, open); err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, found, err := handoffs.Get(ctx, sessionID)
	if err != nil || !found || got.Channel != HandoffChannelTelegram || got.ReplyChannelID != "-1001234567890" || got.ReplyThreadID != "77" {
		t.Fatalf("Get = %+v, %v, %v", got, found, err)
	}
	if list, err := handoffs.ListOpen(ctx); err != nil || len(list) != 1 || list[0].Channel != HandoffChannelTelegram || list[0].ReplyThreadID != "77" {
		t.Fatalf("ListOpen = %+v, %v", list, err)
	}
	released, ok, err := handoffs.Release(ctx, sessionID, opened.Add(time.Hour))
	if err != nil || !ok || released.Channel != HandoffChannelTelegram || released.ReplyThreadID != "77" {
		t.Fatalf("Release = %+v, %v, %v", released, ok, err)
	}
	open.Channel, open.ReplyThreadID = HandoffChannelDiscord, ""
	if err := handoffs.Open(ctx, open); err != nil {
		t.Fatalf("reopen on another channel: %v", err)
	}
	if got, _, _ := handoffs.Get(ctx, sessionID); got.Channel != HandoffChannelDiscord || got.ReplyThreadID != "" {
		t.Fatalf("reopened handoff kept the old target: %+v", got)
	}
}

func TestHandoffOpenRejectsInvalidInput(t *testing.T) {
	db := newTestDB(t)
	sessionID := "lobby/discord:owner/dm:601"
	seedPersonaSession(t, db, sessionID, "discord:601", "lobby")
	handoffs := NewHandoffStore(db)
	valid := Handoff{SessionID: sessionID, Reason: "refund", Channel: HandoffChannelDiscord, OpenedAt: time.Now()}
	tests := []struct {
		name   string
		mutate func(*Handoff)
	}{
		{name: "missing channel", mutate: func(h *Handoff) { h.Channel = "" }},
		{name: "unknown channel", mutate: func(h *Handoff) { h.Channel = "slack" }},
		{name: "missing session", mutate: func(h *Handoff) { h.SessionID = "" }},
		{name: "missing reason", mutate: func(h *Handoff) { h.Reason = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handoff := valid
			tt.mutate(&handoff)
			err := handoffs.Open(t.Context(), &handoff)
			if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
				t.Fatalf("Open err = %v, want %s", err, ErrorCodeInvalidArgument)
			}
		})
	}
	if code, ok := CodeOf(handoffs.Open(t.Context(), nil)); !ok || code != ErrorCodeInvalidArgument {
		t.Fatalf("Open(nil) code = %q, %v", code, ok)
	}
	if list, err := handoffs.ListOpen(t.Context()); err != nil || len(list) != 0 {
		t.Fatalf("rejected handoffs were stored: %+v, %v", list, err)
	}
}

func TestHandoffChannelColumnRejectsUnknownChannels(t *testing.T) {
	db := newTestDB(t)
	sessionID := "lobby/discord:owner/dm:601"
	seedPersonaSession(t, db, sessionID, "discord:601", "lobby")
	_, err := db.ExecContext(t.Context(), `INSERT INTO session_handoff (session_id, reason, opened_at, channel) VALUES (?, 'x', '2026-11-05T09:00:00Z', 'slack')`, sessionID)
	if !isConstraintCheck(err) {
		t.Fatalf("insert with an unknown channel err = %v, want a check violation", err)
	}
}

func TestHandoffChannelMigrationKeepsExistingDiscordRows(t *testing.T) {
	ctx := t.Context()
	db := openDatabaseAtVersion(t, filepath.Join(t.TempDir(), "aura.db"), len(migrations)-1)
	sessionID := "lobby/discord:owner/dm:601"
	seedPersonaSession(t, db, sessionID, "discord:601", "lobby")
	if _, err := db.ExecContext(ctx, `INSERT INTO session_handoff (session_id, reason, opened_at, principal_id, agent_id, account, reply_channel_id) VALUES (?, 'refund', '2026-11-04T09:00:00Z', 'discord:601', 'lobby', 'owner', '4400')`, sessionID); err != nil {
		t.Fatalf("seed v%d handoff: %v", migrations[len(migrations)-2].version, err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	got, found, err := NewHandoffStore(db).Get(ctx, sessionID)
	if err != nil || !found {
		t.Fatalf("Get = %+v, %v, %v", got, found, err)
	}
	if got.Channel != HandoffChannelDiscord || got.ReplyThreadID != "" || got.ReplyChannelID != "4400" || got.Account != "owner" {
		t.Fatalf("migrated handoff = %+v, want a discord row with no thread", got)
	}
}
