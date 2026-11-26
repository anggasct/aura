package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/channels/telegram"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/handoff"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/fault"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

type stubHandoffNotifier struct {
	target handoff.Target
	err    error
	calls  int
}

func (n *stubHandoffNotifier) NotifyHandoff(context.Context, string, string) (handoff.Target, error) {
	n.calls++
	return n.target, n.err
}

func mustHandoffStore(t *testing.T, db *sql.DB) *handoff.SQLiteStore {
	t.Helper()
	s, err := handoff.NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("handoff.NewSQLiteStore: %v", err)
	}
	return s
}

func mustHandoffService(t *testing.T, db *sql.DB, notifiers []handoff.Notifier) *handoff.Service {
	t.Helper()
	svc, err := handoff.NewService(mustHandoffStore(t, db), notifiers, time.Now)
	if err != nil {
		t.Fatalf("handoff.NewService: %v", err)
	}
	return svc
}

func runHandoffSubcommand(t *testing.T, index int, cfg string, args ...string) (string, error) {
	t.Helper()
	restoreDefaultLogger(t)
	cmds := handoff.Commands(handoffOpener(&globalFlags{configPath: cfg}))
	cmd := cmds[index]
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return stdout.String(), err
}

func runReplyCommand(t *testing.T, cfg string, args ...string) (string, error) {
	t.Helper()
	return runHandoffSubcommand(t, 0, cfg, args...)
}

func runHandoffCommand(t *testing.T, cfg string, args ...string) (string, error) {
	t.Helper()
	return runHandoffSubcommand(t, 1, cfg, args...)
}

type deliveredText struct {
	account, channel, text string
}

func captureDeliveries(t *testing.T) *[]deliveredText {
	t.Helper()
	var delivered []deliveredText
	previous := deliverText
	deliverText = func(_ context.Context, account *config.DiscordAccount, channelID, text string) error {
		delivered = append(delivered, deliveredText{account.Name, channelID, text})
		return nil
	}
	t.Cleanup(func() { deliverText = previous })
	return &delivered
}

func TestHandoffServiceRecordsTheRequesterTarget(t *testing.T) {
	cfg := writePrincipalCLIConfig(t)
	db := openAccessDB(t, cfg)
	seedIdleSession(t, turnstore.NewSessionService(db), "main/discord:office/dm:9", "discord:9", time.Now().UTC())
	elsewhere := &stubHandoffNotifier{err: handoff.ErrNoTarget}
	office := &stubHandoffNotifier{target: handoff.Target{Channel: "discord", Account: "office", ChannelID: "4400"}}
	service := mustHandoffService(t, db, []handoff.Notifier{elsewhere, office})
	request := &handoff.Request{SessionID: "main/discord:office/dm:9", PrincipalID: "discord:9", AgentID: "main", Reason: "refund"}
	if err := service.OpenHandoff(t.Context(), request); err != nil {
		t.Fatalf("OpenHandoff: %v", err)
	}
	h, found, err := mustHandoffStore(t, db).Get(t.Context(), request.SessionID)
	if err != nil || !found || h.Account != "office" || h.Channel != handoff.ChannelDiscord || h.ReplyThreadID != "" || h.ReplyChannelID != "4400" || h.Reason != "refund" || elsewhere.calls != 1 {
		t.Fatalf("handoff = %+v, %v, %v", h, found, err)
	}
	now := time.Now()
	gate, err := handoff.NewGate(mustHandoffStore(t, db), time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatalf("handoff.NewGate: %v", err)
	}
	if state, err := gate.Check(t.Context(), request.SessionID); err != nil || state != access.HandoffStateOpen {
		t.Fatalf("gate = %q, %v", state, err)
	}
	now = now.Add(2 * time.Hour)
	if state, _ := gate.Check(t.Context(), request.SessionID); state != access.HandoffStateExpired {
		t.Fatalf("gate after timeout = %q, want expired", state)
	}
	failing := mustHandoffService(t, db, []handoff.Notifier{&stubHandoffNotifier{err: errors.New("dm closed")}})
	if err := failing.OpenHandoff(t.Context(), request); err == nil {
		t.Fatal("handoff opened although the owner was not notified")
	}
	if err := mustHandoffService(t, db, nil).OpenHandoff(t.Context(), request); err == nil {
		t.Fatal("handoff opened without any channel")
	}
}

func TestReplyAndReleaseAHandoff(t *testing.T) {
	cfg := writePrincipalCLIConfig(t)
	db := openAccessDB(t, cfg)
	delivered := captureDeliveries(t)
	session := "main/discord:office/dm:9"
	seedIdleSession(t, turnstore.NewSessionService(db), session, "discord:9", time.Now().UTC())
	if _, err := runReplyCommand(t, cfg, session, "hi"); err == nil || !strings.Contains(err.Error(), "handoff_not_open") {
		t.Fatalf("reply without handoff err = %v", err)
	}
	if err := mustHandoffStore(t, db).Open(t.Context(), &handoff.Handoff{SessionID: session, Reason: "refund", PrincipalID: "discord:9", AgentID: "main", Account: "office", Channel: handoff.ChannelDiscord, ReplyChannelID: "4400", OpenedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	out, err := runHandoffCommand(t, cfg, "list")
	if err != nil || !strings.Contains(out, session+"\tdiscord:9\tmain\t") || !strings.Contains(out, "\trefund") {
		t.Fatalf("list = %q, %v", out, err)
	}
	if _, err := runReplyCommand(t, cfg, session, "Your", "refund", "is", "approved."); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if len(*delivered) != 1 || (*delivered)[0] != (deliveredText{"office", "4400", "Your refund is approved."}) {
		t.Fatalf("delivered = %+v", *delivered)
	}
	events, err := turnstore.NewSessionService(db).ListEvents(t.Context(), session, 0, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %+v, %v", events, err)
	}
	adk, err := turnstore.RuntimeEventToADK(&events[0])
	if err != nil || adk.Author != "Aura" || adk.Content == nil || adk.Content.Role != "model" || adk.Content.Parts[0].Text != "Your refund is approved." {
		t.Fatalf("journaled reply = %+v, %v", adk, err)
	}
	if _, err := runHandoffCommand(t, cfg, "release", session); err != nil {
		t.Fatalf("release: %v", err)
	}
	if len(*delivered) != 2 || (*delivered)[1].text != auraagent.PersonaMessageDefaults()[auraagent.MessageHandoffClosed] {
		t.Fatalf("release notice = %+v", *delivered)
	}
	if _, err := runHandoffCommand(t, cfg, "release", session); err == nil {
		t.Fatal("released twice")
	}
	if accessEventCount(t, cfg, accessActionHandoffReply) != 1 || accessEventCount(t, cfg, accessActionHandoffRelease) != 1 {
		t.Fatal("reply and release were not recorded as owner actions")
	}
}

func TestReplyEmptyTextExitsWithUsageCode(t *testing.T) {
	restoreDefaultLogger(t)
	root := NewRootCommand()
	args := []string{"reply", "main/discord:office/dm:9", "   "}
	root.SetArgs(args)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	err := root.ExecuteContext(t.Context())
	if err == nil || err.Error() != "reply text must not be empty" {
		t.Fatalf("err = %v, want reply text must not be empty", err)
	}
	if code := ExitCode(root, args, err); code != 2 {
		t.Fatalf("ExitCode = %d, want 2", code)
	}
}

type deliveredTelegramText struct {
	account, chat, thread, text string
}

func captureTelegramDeliveries(t *testing.T, failWith error) *[]deliveredTelegramText {
	t.Helper()
	var delivered []deliveredTelegramText
	previous := deliverTelegramText
	deliverTelegramText = func(_ context.Context, account *telegram.Account, chatID, threadID, text string) error {
		if failWith != nil {
			return failWith
		}
		delivered = append(delivered, deliveredTelegramText{account.Name, chatID, threadID, text})
		return nil
	}
	t.Cleanup(func() { deliverTelegramText = previous })
	return &delivered
}

func TestHandoffServiceRecordsATelegramRequesterTarget(t *testing.T) {
	cfg := writeTelegramAccessConfig(t)
	db := openAccessDB(t, cfg)
	session := "main/telegram:pocket/chat:-1001234567890/topic:7"
	seedIdleSession(t, turnstore.NewSessionService(db), session, "telegram:555001", time.Now().UTC())
	discordElsewhere := &stubHandoffNotifier{err: handoff.ErrNoTarget}
	telegramNotifier := &stubHandoffNotifier{target: handoff.Target{Channel: "telegram", Account: "pocket", ChannelID: "-1001234567890", ThreadID: "7"}}
	service := mustHandoffService(t, db, []handoff.Notifier{discordElsewhere, telegramNotifier})
	request := &handoff.Request{SessionID: session, PrincipalID: "telegram:555001", AgentID: "main", Reason: "refund"}
	if err := service.OpenHandoff(t.Context(), request); err != nil {
		t.Fatalf("OpenHandoff: %v", err)
	}
	h, found, err := mustHandoffStore(t, db).Get(t.Context(), session)
	if err != nil || !found {
		t.Fatalf("Get = %+v, %v, %v", h, found, err)
	}
	if h.Channel != handoff.ChannelTelegram || h.Account != "pocket" || h.ReplyChannelID != "-1001234567890" || h.ReplyThreadID != "7" || discordElsewhere.calls != 1 {
		t.Fatalf("handoff = %+v", h)
	}
}

func TestReplyAndReleaseATelegramHandoff(t *testing.T) {
	cfg := writeTelegramAccessConfig(t)
	db := openAccessDB(t, cfg)
	delivered := captureTelegramDeliveries(t, nil)
	session := "main/telegram:pocket/chat:-1001234567890/topic:7"
	seedIdleSession(t, turnstore.NewSessionService(db), session, "telegram:555001", time.Now().UTC())
	if err := mustHandoffStore(t, db).Open(t.Context(), &handoff.Handoff{
		SessionID: session, Reason: "refund", PrincipalID: "telegram:555001", AgentID: "main", Account: "pocket",
		Channel: handoff.ChannelTelegram, ReplyChannelID: "-1001234567890", ReplyThreadID: "7", OpenedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	out, err := runHandoffCommand(t, cfg, "list")
	if err != nil || !strings.Contains(out, session+"\ttelegram:555001\tmain\t") || !strings.Contains(out, "\trefund") {
		t.Fatalf("list = %q, %v", out, err)
	}
	if _, err := runReplyCommand(t, cfg, session, "Your", "refund", "is", "approved."); err != nil {
		t.Fatalf("reply: %v", err)
	}
	want := deliveredTelegramText{"pocket", "-1001234567890", "7", "Your refund is approved."}
	if len(*delivered) != 1 || (*delivered)[0] != want {
		t.Fatalf("delivered = %+v, want %+v", *delivered, want)
	}
	events, err := turnstore.NewSessionService(db).ListEvents(t.Context(), session, 0, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %+v, %v", events, err)
	}
	if adk, err := turnstore.RuntimeEventToADK(&events[0]); err != nil || adk.Content == nil || adk.Content.Parts[0].Text != "Your refund is approved." {
		t.Fatalf("journaled reply = %+v, %v", adk, err)
	}
	if _, err := runHandoffCommand(t, cfg, "release", session); err != nil {
		t.Fatalf("release: %v", err)
	}
	closed := deliveredTelegramText{"pocket", "-1001234567890", "7", auraagent.PersonaMessageDefaults()[auraagent.MessageHandoffClosed]}
	if len(*delivered) != 2 || (*delivered)[1] != closed {
		t.Fatalf("release notice = %+v, want %+v", *delivered, closed)
	}
	if accessEventCount(t, cfg, accessActionHandoffReply) != 1 || accessEventCount(t, cfg, accessActionHandoffRelease) != 1 {
		t.Fatal("reply and release were not recorded as owner actions")
	}
}

func TestReplyToATelegramHandoffFailsWithoutJournaling(t *testing.T) {
	session := "main/telegram:pocket/chat:555001"
	open := func(t *testing.T, cfg, account, chatID string) {
		t.Helper()
		db := openAccessDB(t, cfg)
		seedIdleSession(t, turnstore.NewSessionService(db), session, "telegram:555001", time.Now().UTC())
		if err := mustHandoffStore(t, db).Open(t.Context(), &handoff.Handoff{
			SessionID: session, Reason: "refund", PrincipalID: "telegram:555001", AgentID: "main", Account: account,
			Channel: handoff.ChannelTelegram, ReplyChannelID: chatID, OpenedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("account that is not configured", func(t *testing.T) {
		cfg := writeTelegramAccessConfig(t)
		open(t, cfg, "ghost", "555001")
		delivered := captureTelegramDeliveries(t, nil)
		_, err := runReplyCommand(t, cfg, session, "hi")
		if err == nil || !strings.Contains(err.Error(), `handoff account "ghost" is not configured`) || len(*delivered) != 0 {
			t.Fatalf("err = %v, delivered = %+v", err, *delivered)
		}
	})
	t.Run("delivery that fails", func(t *testing.T) {
		cfg := writeTelegramAccessConfig(t)
		open(t, cfg, "pocket", "555001")
		captureTelegramDeliveries(t, errors.New("telegram down"))
		if _, err := runReplyCommand(t, cfg, session, "hi"); err == nil || !strings.Contains(err.Error(), "telegram down") {
			t.Fatalf("err = %v", err)
		}
		if accessEventCount(t, cfg, accessActionHandoffReply) != 0 {
			t.Fatal("a reply that never arrived was recorded")
		}
	})
	t.Run("stored chat id that is corrupt", func(t *testing.T) {
		cfg := writeTelegramAccessConfig(t)
		open(t, cfg, "pocket", "not-a-chat")
		_, err := runReplyCommand(t, cfg, session, "hi")
		if !fault.HasCode(err, telegram.ErrorCodeInvalidArgument) {
			t.Fatalf("err = %v, want %s", err, telegram.ErrorCodeInvalidArgument)
		}
		if accessEventCount(t, cfg, accessActionHandoffReply) != 0 {
			t.Fatal("a reply that never arrived was recorded")
		}
	})
}
