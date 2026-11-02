package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/channel/discord"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/store"
	toolsbuiltin "github.com/anggasct/aura/internal/tools/builtin"
)

type stubHandoffNotifier struct {
	target discord.HandoffTarget
	err    error
	calls  int
}

func (n *stubHandoffNotifier) NotifyHandoff(context.Context, string, string) (discord.HandoffTarget, error) {
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
	elsewhere := &stubHandoffNotifier{err: discord.ErrNoHandoffTarget}
	office := &stubHandoffNotifier{target: discord.HandoffTarget{Account: "office", ChannelID: "4400"}}
	service := newHandoffService(db, []handoffNotifier{elsewhere, office})
	request := &toolsbuiltin.HandoffRequest{SessionID: "main/discord:office/dm:9", PrincipalID: "discord:9", AgentID: "main", Reason: "refund"}
	if err := service.OpenHandoff(t.Context(), request); err != nil {
		t.Fatalf("OpenHandoff: %v", err)
	}
	handoff, found, err := store.NewHandoffStore(db).Get(t.Context(), request.SessionID)
	if err != nil || !found || handoff.Account != "office" || handoff.ReplyChannelID != "4400" || handoff.Reason != "refund" || elsewhere.calls != 1 {
		t.Fatalf("handoff = %+v, %v, %v", handoff, found, err)
	}
	gate := &handoffGate{handoffs: store.NewHandoffStore(db), timeout: time.Hour, now: time.Now}
	if state, err := gate.Check(t.Context(), request.SessionID); err != nil || state != discord.HandoffStateOpen {
		t.Fatalf("gate = %q, %v", state, err)
	}
	gate.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if state, _ := gate.Check(t.Context(), request.SessionID); state != discord.HandoffStateExpired {
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
	if err := store.NewHandoffStore(db).Open(t.Context(), &store.Handoff{SessionID: session, Reason: "refund", PrincipalID: "discord:9", AgentID: "main", Account: "office", ReplyChannelID: "4400", OpenedAt: time.Now().UTC()}); err != nil {
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
