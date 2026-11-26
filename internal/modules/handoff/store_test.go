package handoff

import (
	"database/sql"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

func seedPersonaSession(t *testing.T, db *sql.DB, sessionID, ownerID, agentID string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO session (id, owner_id, agent_id, metadata_json, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
		sessionID, ownerID, agentID, `{}`, "2026-09-12T12:00:00Z", "2026-09-12T12:00:00Z"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

func mustNewSQLiteStore(t *testing.T, db *sql.DB) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	return s
}

func TestHandoffOpenCheckReleaseAndTimeout(t *testing.T) {
	if _, err := NewSQLiteStore(nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("NewSQLiteStore(nil) = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), Schema())
	ctx := t.Context()
	seedPersonaSession(t, db, "lobby/discord:owner/dm:601", "discord:601", "lobby")
	handoffs := mustNewSQLiteStore(t, db)
	opened := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	if state, _, err := handoffs.Check(ctx, "lobby/discord:owner/dm:601", time.Hour, opened); err != nil || state != StateNone {
		t.Fatalf("Check before open = %q, %v", state, err)
	}
	open := &Handoff{SessionID: "lobby/discord:owner/dm:601", Reason: "refund request", PrincipalID: "discord:601", AgentID: "lobby", Account: "owner", Channel: ChannelDiscord, ReplyChannelID: "4400", OpenedAt: opened}
	if err := handoffs.Open(ctx, open); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if state, got, err := handoffs.Check(ctx, open.SessionID, time.Hour, opened.Add(59*time.Minute)); err != nil || state != StateOpen || got.ReplyChannelID != "4400" || got.Account != "owner" {
		t.Fatalf("Check inside timeout = %q, %+v, %v", state, got, err)
	}
	if list, err := handoffs.ListOpen(ctx); err != nil || len(list) != 1 || list[0].Reason != "refund request" {
		t.Fatalf("ListOpen = %+v, %v", list, err)
	}
	if state, got, err := handoffs.Check(ctx, open.SessionID, time.Hour, opened.Add(time.Hour)); err != nil || state != StateExpired || got.ReleasedAt.IsZero() {
		t.Fatalf("Check at timeout = %q, %+v, %v; want auto-release", state, got, err)
	}
	if state, _, _ := handoffs.Check(ctx, open.SessionID, time.Hour, opened.Add(2*time.Hour)); state != StateNone {
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
	if err := handoffs.Open(ctx, &Handoff{SessionID: open.SessionID, Channel: ChannelDiscord}); err == nil {
		t.Fatal("handoff without a reason accepted")
	}
}

func TestHandoffRoundTripsATelegramReplyTarget(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), Schema())
	ctx := t.Context()
	sessionID := "lobby/telegram:pocket/chat:555001"
	seedPersonaSession(t, db, sessionID, "telegram:555001", "lobby")
	handoffs := mustNewSQLiteStore(t, db)
	opened := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	open := &Handoff{
		SessionID: sessionID, Reason: "refund request", PrincipalID: "telegram:555001", AgentID: "lobby",
		Account: "pocket", Channel: ChannelTelegram, ReplyChannelID: "-1001234567890", ReplyThreadID: "77", OpenedAt: opened,
	}
	if err := handoffs.Open(ctx, open); err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, found, err := handoffs.Get(ctx, sessionID)
	if err != nil || !found || got.Channel != ChannelTelegram || got.ReplyChannelID != "-1001234567890" || got.ReplyThreadID != "77" {
		t.Fatalf("Get = %+v, %v, %v", got, found, err)
	}
	if list, err := handoffs.ListOpen(ctx); err != nil || len(list) != 1 || list[0].Channel != ChannelTelegram || list[0].ReplyThreadID != "77" {
		t.Fatalf("ListOpen = %+v, %v", list, err)
	}
	released, ok, err := handoffs.Release(ctx, sessionID, opened.Add(time.Hour))
	if err != nil || !ok || released.Channel != ChannelTelegram || released.ReplyThreadID != "77" {
		t.Fatalf("Release = %+v, %v, %v", released, ok, err)
	}
	open.Channel, open.ReplyThreadID = ChannelDiscord, ""
	if err := handoffs.Open(ctx, open); err != nil {
		t.Fatalf("reopen on another channel: %v", err)
	}
	if got, _, _ := handoffs.Get(ctx, sessionID); got.Channel != ChannelDiscord || got.ReplyThreadID != "" {
		t.Fatalf("reopened handoff kept the old target: %+v", got)
	}
}

func TestHandoffRoundTripsAnHTTPTarget(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), Schema())
	ctx := t.Context()
	sessionID := "lobby/http/abcdefghijkl/token"
	seedPersonaSession(t, db, sessionID, "http:abcdefghijkl", "lobby")
	handoffs := mustNewSQLiteStore(t, db)
	opened := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	open := &Handoff{SessionID: sessionID, Reason: "refund", PrincipalID: "http:abcdefghijkl", AgentID: "lobby", Account: "abcdefghijkl", Channel: ChannelHTTP, OpenedAt: opened}
	if err := handoffs.Open(ctx, open); err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, found, err := handoffs.Get(ctx, sessionID)
	if err != nil || !found || got.Channel != ChannelHTTP || got.Account != "abcdefghijkl" || got.ReplyChannelID != "" || got.ReplyThreadID != "" {
		t.Fatalf("Get = %+v, %v, %v", got, found, err)
	}
}

func TestHandoffOpenRejectsInvalidInput(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), Schema())
	sessionID := "lobby/discord:owner/dm:601"
	seedPersonaSession(t, db, sessionID, "discord:601", "lobby")
	handoffs := mustNewSQLiteStore(t, db)
	valid := Handoff{SessionID: sessionID, Reason: "refund", Channel: ChannelDiscord, OpenedAt: time.Now()}
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
			if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeInvalidArgument {
				t.Fatalf("Open err = %v, want %s", err, ErrorCodeInvalidArgument)
			}
		})
	}
	if code, ok := fault.CodeOf[ErrorCode](handoffs.Open(t.Context(), nil)); !ok || code != ErrorCodeInvalidArgument {
		t.Fatalf("Open(nil) code = %q, %v", code, ok)
	}
	if list, err := handoffs.ListOpen(t.Context()); err != nil || len(list) != 0 {
		t.Fatalf("rejected handoffs were stored: %+v, %v", list, err)
	}
}

func TestHandoffChannelColumnRejectsUnknownChannels(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), Schema())
	sessionID := "lobby/discord:owner/dm:601"
	seedPersonaSession(t, db, sessionID, "discord:601", "lobby")
	_, err := db.ExecContext(t.Context(), `INSERT INTO session_handoff (session_id, reason, opened_at, channel) VALUES (?, 'x', '2026-11-05T09:00:00Z', 'slack')`, sessionID)
	if !fault.HasCode(sqlite.Classify(err), sqlite.ErrorCodeConstraintCheck) {
		t.Fatalf("insert with an unknown channel err = %v, want a check violation", err)
	}
}

func TestHandoffBaselineSchemaChannelsDefaultsAndCascade(t *testing.T) {
	ctx := t.Context()
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), Schema())
	defaultSession := "lobby/discord:owner/dm:601"
	seedPersonaSession(t, db, defaultSession, "discord:601", "lobby")
	if _, err := db.ExecContext(ctx, `INSERT INTO session_handoff (session_id, reason, opened_at, principal_id, agent_id, account, reply_channel_id) VALUES (?, 'refund', '2026-11-04T09:00:00Z', 'discord:601', 'lobby', 'owner', '4400')`, defaultSession); err != nil {
		t.Fatalf("seed default channel handoff: %v", err)
	}
	handoffs := mustNewSQLiteStore(t, db)
	gotDefault, found, err := handoffs.Get(ctx, defaultSession)
	if err != nil || !found {
		t.Fatalf("Get default = %+v, %v, %v", gotDefault, found, err)
	}
	if gotDefault.Channel != ChannelDiscord || gotDefault.ReplyThreadID != "" || gotDefault.ReplyChannelID != "4400" || gotDefault.Account != "owner" {
		t.Fatalf("default handoff = %+v, want discord row with empty thread", gotDefault)
	}

	var indexes int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'session_handoff_open_idx' AND tbl_name = 'session_handoff'`).Scan(&indexes); err != nil || indexes != 1 {
		t.Fatalf("open index count = %d, %v; want 1", indexes, err)
	}

	channels := []struct {
		session, channel, account, replyChannel, replyThread string
	}{
		{"lobby/telegram:pocket/chat:555001", ChannelTelegram, "pocket", "-1001234567890", "77"},
		{"lobby/http/abcdefghijkl/token", ChannelHTTP, "abcdefghijkl", "", ""},
		{"lobby/email:desk/thread:abcdefghijklmnopqrstuvwxyz", ChannelEmail, "desk", "alice@example.com", ""},
	}
	for _, row := range channels {
		t.Run(row.channel, func(t *testing.T) {
			seedPersonaSession(t, db, row.session, "owner-1", "lobby")
			if err := handoffs.Open(t.Context(), &Handoff{
				SessionID:      row.session,
				Reason:         "refund",
				PrincipalID:    "owner-1",
				AgentID:        "lobby",
				Account:        row.account,
				Channel:        row.channel,
				ReplyChannelID: row.replyChannel,
				ReplyThreadID:  row.replyThread,
				OpenedAt:       time.Now(),
			}); err != nil {
				t.Fatalf("Open %s handoff: %v", row.channel, err)
			}
			got, found, err := handoffs.Get(t.Context(), row.session)
			if err != nil || !found || got.Channel != row.channel || got.ReplyChannelID != row.replyChannel || got.ReplyThreadID != row.replyThread {
				t.Fatalf("Get = %+v, %v, %v", got, found, err)
			}
			if _, err := db.ExecContext(t.Context(), `UPDATE session_handoff SET channel = 'slack' WHERE session_id = ?`, row.session); !fault.HasCode(sqlite.Classify(err), sqlite.ErrorCodeConstraintCheck) {
				t.Fatalf("update to unknown channel err = %v, want check violation", err)
			}
			if _, err := db.ExecContext(t.Context(), `DELETE FROM session WHERE id = ?`, row.session); err != nil {
				t.Fatalf("delete session: %v", err)
			}
			if _, found, err := handoffs.Get(t.Context(), row.session); err != nil || found {
				t.Fatalf("handoff survived its session: found=%v err=%v", found, err)
			}
		})
	}
}
