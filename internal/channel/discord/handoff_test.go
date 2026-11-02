package discord

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
)

type fakeHandoffGate struct {
	mu    sync.Mutex
	state string
	calls []string
}

func (g *fakeHandoffGate) Check(_ context.Context, sessionID string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, sessionID)
	return g.state, nil
}

func newHandoffAdapter(t *testing.T, gate *fakeHandoffGate) (*Adapter, *memorySink, *approvalStub) {
	t.Helper()
	stub := newApprovalStub(t)
	adapter, sink, observer := newAccessAdapter(t, "lobby", []string{"discord:777"}, accessPersonas())
	settings := observer.settings()
	settings.Handoffs = gate
	adapter.SetAccess(&settings)
	adapter.restBase = stub.srv.URL
	return adapter, sink, stub
}

func postContents(stub *approvalStub, channel string) []string {
	var contents []string
	for _, post := range stub.postsTo(channel) {
		text, _ := post.body["content"].(string)
		contents = append(contents, text)
	}
	return contents
}

func TestNotifyHandoffTellsTheOwnerAndTheRequester(t *testing.T) {
	gate := &fakeHandoffGate{state: HandoffStateNone}
	adapter, sink, stub := newHandoffAdapter(t, gate)
	if admitted, err := adapter.admit(t.Context(), textMessage("9901", "4400", "", "601", "halo", nil)); err != nil || !admitted {
		t.Fatalf("admit = %v, %v", admitted, err)
	}
	session := routedEnvelope(t, sink, "message:9901").ConversationID
	target, err := adapter.NotifyHandoff(t.Context(), session, "wants a refund")
	if err != nil || target != (HandoffTarget{Account: "owner", ChannelID: "4400"}) {
		t.Fatalf("NotifyHandoff = %+v, %v", target, err)
	}
	owner := strings.Join(postContents(stub, "555"), "\n")
	for _, want := range []string{"handoff requested", "session: " + session, "principal: discord:601", "reason: wants a refund", "aura reply " + session, "aura handoff release " + session} {
		if !strings.Contains(owner, want) {
			t.Errorf("owner notice %q lacks %q", owner, want)
		}
	}
	if got := postContents(stub, "4400"); len(got) != 1 || got[0] != config.PersonaMessageDefaults()[config.MessageHandoffOpen] {
		t.Fatalf("requester notices = %q, want handoff_open", got)
	}
	if _, err := adapter.NotifyHandoff(t.Context(), "lobby/discord:owner/dm:unknown", "x"); !errors.Is(err, ErrNoHandoffTarget) {
		t.Fatalf("unknown session err = %v, want ErrNoHandoffTarget", err)
	}
}

func TestOpenHandoffForwardsMessagesInsteadOfStartingTurns(t *testing.T) {
	gate := &fakeHandoffGate{state: HandoffStateOpen}
	adapter, sink, stub := newHandoffAdapter(t, gate)
	if handled, err := adapter.admit(t.Context(), textMessage("9911", "4400", "", "601", "are you still there?", nil)); err != nil || !handled {
		t.Fatalf("admit = %v, %v; want handled", handled, err)
	}
	if sink.calls["message:9911"] != 0 {
		t.Fatal("held message started a turn")
	}
	if owner := strings.Join(postContents(stub, "555"), "\n"); !strings.Contains(owner, "discord:601: are you still there?") || !strings.Contains(owner, "lobby/discord:owner/dm:601") {
		t.Fatalf("owner forward = %q", owner)
	}
	calls := len(gate.calls)
	if admitted, err := adapter.admit(t.Context(), textMessage("9912", "7770", "", "777", "owner here", nil)); err != nil || !admitted {
		t.Fatalf("owner admit = %v, %v", admitted, err)
	}
	if len(gate.calls) != calls {
		t.Fatal("owner messages consulted the handoff gate")
	}
	gate.state = HandoffStateExpired
	if admitted, err := adapter.admit(t.Context(), textMessage("9913", "4400", "", "601", "hello again", nil)); err != nil || !admitted {
		t.Fatalf("admit after timeout = %v, %v; want a normal turn", admitted, err)
	}
	if got := postContents(stub, "4400"); len(got) != 1 || got[0] != config.PersonaMessageDefaults()[config.MessageHandoffClosed] {
		t.Fatalf("requester notices = %q, want handoff_closed", got)
	}
	routedEnvelope(t, sink, "message:9913")
}

func TestReplyTargetNamesTheConversationChannel(t *testing.T) {
	adapter, _, _ := newAccessAdapter(t, "team", []string{"discord:777"}, accessPersonas())
	if _, ok := adapter.ReplyTarget("dm:111"); ok {
		t.Fatal("an unseen conversation has a reply target")
	}
	adapter.rememberTarget("dm:111", &approvalBinding{principal: "discord:111", class: principal.ClassTrusted, persona: "team", actor: "111", channelID: "333"})
	target, ok := adapter.ReplyTarget("dm:111")
	if !ok || target.ChannelID != "333" || target.Account != adapter.account.Name {
		t.Fatalf("ReplyTarget = %+v, %v", target, ok)
	}
}
