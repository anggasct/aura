package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/channels/telegram"
	"github.com/anggasct/aura/internal/config"
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/store"
)

type stubHandoffNotifier struct {
	target access.HandoffTarget
	err    error
	calls  int
}

func (n *stubHandoffNotifier) NotifyHandoff(context.Context, string, string) (access.HandoffTarget, error) {
	n.calls++
	return n.target, n.err
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
	seedIdleSession(t, store.NewSessionService(db), "main/discord:office/dm:9", "discord:9", time.Now().UTC())
	elsewhere := &stubHandoffNotifier{err: access.ErrNoHandoffTarget}
	office := &stubHandoffNotifier{target: access.HandoffTarget{Channel: "discord", Account: "office", ChannelID: "4400"}}
	service := newHandoffService(db, []handoffNotifier{elsewhere, office})
	request := &toolsbuiltin.HandoffRequest{SessionID: "main/discord:office/dm:9", PrincipalID: "discord:9", AgentID: "main", Reason: "refund"}
	if err := service.OpenHandoff(t.Context(), request); err != nil {
		t.Fatalf("OpenHandoff: %v", err)
	}
	handoff, found, err := store.NewHandoffStore(db).Get(t.Context(), request.SessionID)
	if err != nil || !found || handoff.Account != "office" || handoff.Channel != store.HandoffChannelDiscord || handoff.ReplyThreadID != "" || handoff.ReplyChannelID != "4400" || handoff.Reason != "refund" || elsewhere.calls != 1 {
		t.Fatalf("handoff = %+v, %v, %v", handoff, found, err)
	}
	gate := &handoffGate{handoffs: store.NewHandoffStore(db), timeout: time.Hour, now: time.Now}
	if state, err := gate.Check(t.Context(), request.SessionID); err != nil || state != access.HandoffStateOpen {
		t.Fatalf("gate = %q, %v", state, err)
	}
	gate.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if state, _ := gate.Check(t.Context(), request.SessionID); state != access.HandoffStateExpired {
		t.Fatalf("gate after timeout = %q, want expired", state)
	}
	failing := newHandoffService(db, []handoffNotifier{&stubHandoffNotifier{err: errors.New("dm closed")}})
	if err := failing.OpenHandoff(t.Context(), request); err == nil {
		t.Fatal("handoff opened although the owner was not notified")
	}
	if err := newHandoffService(db, nil).OpenHandoff(t.Context(), request); err == nil {
		t.Fatal("handoff opened without any channel")
	}
}

func TestReplyAndReleaseAHandoff(t *testing.T) {
	cfg := writePrincipalCLIConfig(t)
	db := openAccessDB(t, cfg)
	delivered := captureDeliveries(t)
	session := "main/discord:office/dm:9"
	seedIdleSession(t, store.NewSessionService(db), session, "discord:9", time.Now().UTC())
	if _, err := runAccessCommand(t, newReplyCmd, cfg, session, "hi"); err == nil || !strings.Contains(err.Error(), "handoff_not_open") {
		t.Fatalf("reply without handoff err = %v", err)
	}
	if err := store.NewHandoffStore(db).Open(t.Context(), &store.Handoff{SessionID: session, Reason: "refund", PrincipalID: "discord:9", AgentID: "main", Account: "office", Channel: store.HandoffChannelDiscord, ReplyChannelID: "4400", OpenedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	out, err := runAccessCommand(t, newHandoffCmd, cfg, "list")
	if err != nil || !strings.Contains(out, session+"\tdiscord:9\tmain\t") || !strings.Contains(out, "\trefund") {
		t.Fatalf("list = %q, %v", out, err)
	}
	if _, err := runAccessCommand(t, newReplyCmd, cfg, session, "Your", "refund", "is", "approved."); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if len(*delivered) != 1 || (*delivered)[0] != (deliveredText{"office", "4400", "Your refund is approved."}) {
		t.Fatalf("delivered = %+v", *delivered)
	}
	events, err := store.NewSessionService(db).ListEvents(t.Context(), session, 0, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %+v, %v", events, err)
	}
	adk, err := store.RuntimeEventToADK(&events[0])
	if err != nil || adk.Author != "Aura" || adk.Content == nil || adk.Content.Role != "model" || adk.Content.Parts[0].Text != "Your refund is approved." {
		t.Fatalf("journaled reply = %+v, %v", adk, err)
	}
	if _, err := runAccessCommand(t, newHandoffCmd, cfg, "release", session); err != nil {
		t.Fatalf("release: %v", err)
	}
	if len(*delivered) != 2 || (*delivered)[1].text != config.PersonaMessageDefaults()[config.MessageHandoffClosed] {
		t.Fatalf("release notice = %+v", *delivered)
	}
	if _, err := runAccessCommand(t, newHandoffCmd, cfg, "release", session); err == nil {
		t.Fatal("released twice")
	}
	if accessEventCount(t, cfg, accessActionHandoffReply) != 1 || accessEventCount(t, cfg, accessActionHandoffRelease) != 1 {
		t.Fatal("reply and release were not recorded as owner actions")
	}
}

type deliveredTelegramText struct {
	account, chat, thread, text string
}

func captureTelegramDeliveries(t *testing.T, failWith error) *[]deliveredTelegramText {
	t.Helper()
	var delivered []deliveredTelegramText
	previous := deliverTelegramText
	deliverTelegramText = func(_ context.Context, account *config.TelegramAccount, chatID, threadID, text string) error {
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
	seedIdleSession(t, store.NewSessionService(db), session, "telegram:555001", time.Now().UTC())
	discordElsewhere := &stubHandoffNotifier{err: access.ErrNoHandoffTarget}
	telegramNotifier := &stubHandoffNotifier{target: access.HandoffTarget{Channel: "telegram", Account: "pocket", ChannelID: "-1001234567890", ThreadID: "7"}}
	service := newHandoffService(db, []handoffNotifier{discordElsewhere, telegramNotifier})
	request := &toolsbuiltin.HandoffRequest{SessionID: session, PrincipalID: "telegram:555001", AgentID: "main", Reason: "refund"}
	if err := service.OpenHandoff(t.Context(), request); err != nil {
		t.Fatalf("OpenHandoff: %v", err)
	}
	handoff, found, err := store.NewHandoffStore(db).Get(t.Context(), session)
	if err != nil || !found {
		t.Fatalf("Get = %+v, %v, %v", handoff, found, err)
	}
	if handoff.Channel != store.HandoffChannelTelegram || handoff.Account != "pocket" || handoff.ReplyChannelID != "-1001234567890" || handoff.ReplyThreadID != "7" || discordElsewhere.calls != 1 {
		t.Fatalf("handoff = %+v", handoff)
	}
}

func TestReplyAndReleaseATelegramHandoff(t *testing.T) {
	cfg := writeTelegramAccessConfig(t)
	db := openAccessDB(t, cfg)
	delivered := captureTelegramDeliveries(t, nil)
	session := "main/telegram:pocket/chat:-1001234567890/topic:7"
	seedIdleSession(t, store.NewSessionService(db), session, "telegram:555001", time.Now().UTC())
	if err := store.NewHandoffStore(db).Open(t.Context(), &store.Handoff{
		SessionID: session, Reason: "refund", PrincipalID: "telegram:555001", AgentID: "main", Account: "pocket",
		Channel: store.HandoffChannelTelegram, ReplyChannelID: "-1001234567890", ReplyThreadID: "7", OpenedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	out, err := runAccessCommand(t, newHandoffCmd, cfg, "list")
	if err != nil || !strings.Contains(out, session+"\ttelegram:555001\tmain\t") || !strings.Contains(out, "\trefund") {
		t.Fatalf("list = %q, %v", out, err)
	}
	if _, err := runAccessCommand(t, newReplyCmd, cfg, session, "Your", "refund", "is", "approved."); err != nil {
		t.Fatalf("reply: %v", err)
	}
	want := deliveredTelegramText{"pocket", "-1001234567890", "7", "Your refund is approved."}
	if len(*delivered) != 1 || (*delivered)[0] != want {
		t.Fatalf("delivered = %+v, want %+v", *delivered, want)
	}
	events, err := store.NewSessionService(db).ListEvents(t.Context(), session, 0, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %+v, %v", events, err)
	}
	if adk, err := store.RuntimeEventToADK(&events[0]); err != nil || adk.Content == nil || adk.Content.Parts[0].Text != "Your refund is approved." {
		t.Fatalf("journaled reply = %+v, %v", adk, err)
	}
	if _, err := runAccessCommand(t, newHandoffCmd, cfg, "release", session); err != nil {
		t.Fatalf("release: %v", err)
	}
	closed := deliveredTelegramText{"pocket", "-1001234567890", "7", config.PersonaMessageDefaults()[config.MessageHandoffClosed]}
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
		seedIdleSession(t, store.NewSessionService(db), session, "telegram:555001", time.Now().UTC())
		if err := store.NewHandoffStore(db).Open(t.Context(), &store.Handoff{
			SessionID: session, Reason: "refund", PrincipalID: "telegram:555001", AgentID: "main", Account: account,
			Channel: store.HandoffChannelTelegram, ReplyChannelID: chatID, OpenedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("account that is not configured", func(t *testing.T) {
		cfg := writeTelegramAccessConfig(t)
		open(t, cfg, "ghost", "555001")
		delivered := captureTelegramDeliveries(t, nil)
		_, err := runAccessCommand(t, newReplyCmd, cfg, session, "hi")
		if err == nil || !strings.Contains(err.Error(), `handoff account "ghost" is not configured`) || len(*delivered) != 0 {
			t.Fatalf("err = %v, delivered = %+v", err, *delivered)
		}
	})
	t.Run("delivery that fails", func(t *testing.T) {
		cfg := writeTelegramAccessConfig(t)
		open(t, cfg, "pocket", "555001")
		captureTelegramDeliveries(t, errors.New("telegram down"))
		if _, err := runAccessCommand(t, newReplyCmd, cfg, session, "hi"); err == nil || !strings.Contains(err.Error(), "telegram down") {
			t.Fatalf("err = %v", err)
		}
		if accessEventCount(t, cfg, accessActionHandoffReply) != 0 {
			t.Fatal("a reply that never arrived was recorded")
		}
	})
	t.Run("stored chat id that is corrupt", func(t *testing.T) {
		cfg := writeTelegramAccessConfig(t)
		open(t, cfg, "pocket", "not-a-chat")
		_, err := runAccessCommand(t, newReplyCmd, cfg, session, "hi")
		if code, ok := telegram.CodeOf(err); !ok || code != telegram.ErrorCodeInvalidArgument {
			t.Fatalf("err = %v, want %s", err, telegram.ErrorCodeInvalidArgument)
		}
		if accessEventCount(t, cfg, accessActionHandoffReply) != 0 {
			t.Fatal("a reply that never arrived was recorded")
		}
	})
}
