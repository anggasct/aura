package telegram

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/platform/command"
)

type operationSpy struct {
	mu      sync.Mutex
	planned [][]string
	callers []command.Caller
}

func (s *operationSpy) Plan(_ string, tokens []string) command.OperationPlan {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.planned = append(s.planned, slices.Clone(tokens))
	return command.OperationPlan{Operation: "cron.list", Input: json.RawMessage(`{}`)}
}

func (s *operationSpy) Run(_ context.Context, run *command.OperationRun) command.OperationReply {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callers = append(s.callers, *run.Caller)
	return command.OperationReply{Lines: []string{"ID\tNAME", "j1\tnightly"}}
}

func (s *operationSpy) counts() (planned, ran int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.planned), len(s.callers)
}

const botHandle = "@" + testBotName

func newOperationCommandRig(t *testing.T, opts accessOpts, spy *operationSpy) *commandRig {
	t.Helper()
	binding := CommandBinding()
	binding.Names = append(binding.Names, "cron")
	group := command.Definition{Name: "cron", Usage: "<verb> [arguments]", Summary: "Manage scheduled recurring jobs", Audience: command.AudienceOwner, Kind: command.KindOperation, MinArgs: 1, MaxArgs: command.Variadic}
	registry, err := command.NewRegistry(append(command.Catalog(), group), binding)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	sessions := &commandSessions{}
	dispatcher, err := command.NewDispatcher(registry, command.Ports{Sessions: sessions, Skills: pinningSkills{}, Operations: spy}, nil)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	opts.commands = dispatcher
	rig := newAccessRig(t, opts)
	rig.adapter.sink = rig.sink
	return &commandRig{accessRig: rig, sessions: sessions}
}

func TestOperationCommandsRunOnlyInAPrivateChatWithTheOwner(t *testing.T) {
	spy := &operationSpy{}
	rig := newOperationCommandRig(t, accessOpts{chats: groupChats}, spy)
	rig.knowBot()

	rig.handle(t, privateText(10, ownerUserID, `/cron list "a b"`))
	if replies := sentTo(rig.h.api, ownerUserID); len(replies) != 1 || replies[0] != "ID\tNAME\nj1\tnightly" {
		t.Fatalf("private replies = %q, want the operation rows", replies)
	}
	planned, ran := spy.counts()
	if ran != 1 || !slices.Equal(spy.planned[0], []string{"list", "a b"}) || !spy.callers[0].Direct {
		t.Fatalf("planned %d ran %d callers %+v, want one direct run", planned, ran, spy.callers)
	}

	rig.handle(t, commandEntity(groupMessage(11, ownerUserID, "/cron"+botHandle+" list"), "/cron"+botHandle))
	replies := sentTo(rig.h.api, groupChatID)
	if len(replies) != 1 || replies[0] != command.ReplyDirectOnly {
		t.Fatalf("group replies = %q, want only the scope refusal", replies)
	}
	if again, ranAgain := spy.counts(); again != planned || ranAgain != ran {
		t.Errorf("after the group message planned %d ran %d, want no further work than %d and %d", again, ranAgain, planned, ran)
	}
}

func TestOperationCommandsAreDeniedToOtherPrincipalsWithoutTouchingOperations(t *testing.T) {
	spy := &operationSpy{}
	rig := newOperationCommandRig(t, accessOpts{allowed: []string{strconv.Itoa(allowedUserID)}}, spy)

	rig.handle(t, privateText(10, allowedUserID, "/cron list"))

	if replies := sentTo(rig.h.api, allowedUserID); len(replies) != 1 || replies[0] != command.ReplyDenied {
		t.Fatalf("replies = %q, want only the denial", replies)
	}
	if planned, ran := spy.counts(); planned != 0 || ran != 0 {
		t.Errorf("planned %d ran %d for a trusted principal, want none", planned, ran)
	}
}

func TestOperationCommandsAreListedInHelpOnlyForTheOwnerInAPrivateChat(t *testing.T) {
	rig := newOperationCommandRig(t, accessOpts{allowed: []string{strconv.Itoa(allowedUserID)}, chats: groupChats}, &operationSpy{})
	rig.knowBot()

	rig.handle(t, privateText(10, ownerUserID, "/help"))
	rig.handle(t, commandEntity(groupMessage(11, ownerUserID, "/help"+botHandle), "/help"+botHandle))
	rig.handle(t, privateText(12, allowedUserID, "/help"))

	owner, group, trusted := sentTo(rig.h.api, ownerUserID), sentTo(rig.h.api, groupChatID), sentTo(rig.h.api, allowedUserID)
	if len(owner) != 1 || !strings.Contains(owner[0], "/cron &lt;verb&gt; [arguments]") || !strings.Contains(owner[0], "Manage scheduled recurring jobs") {
		t.Errorf("owner help = %q, want the cron group", owner)
	}
	for name, replies := range map[string][]string{"group": group, "trusted": trusted} {
		if len(replies) != 1 || strings.Contains(replies[0], "/cron") {
			t.Errorf("%s help = %q, want no operation group", name, replies)
		}
	}
}

func TestTelegramMenuListsOperationGroups(t *testing.T) {
	rig := newOperationCommandRig(t, accessOpts{}, &operationSpy{})
	startAndAwaitPoll(t, rig)

	got := menuCalls(t, rig)

	want := []menuEntry{
		{scope: "default", command: []string{"help", "forgetme"}},
		{scope: "chat", chatID: ownerUserID, command: []string{"help", "status", "session", "new", "cancel", "skill", "skill_create", "cron"}},
	}
	if len(got) != len(want) {
		t.Fatalf("setMyCommands calls = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].scope != want[i].scope || got[i].chatID != want[i].chatID || !slices.Equal(got[i].command, want[i].command) {
			t.Errorf("setMyCommands call %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestMenuStaysWithinTheCommandLimit(t *testing.T) {
	var bound []command.Definition
	for i := range maxMenuCommands + 20 {
		bound = append(bound, command.Definition{Name: "group" + strconv.Itoa(i), Summary: "a group", Audience: command.AudienceOwner, Kind: command.KindOperation})
	}
	menu := menuCommands(bound, command.AudienceOwner)
	if len(menu) != maxMenuCommands || menu[0].Command != "group0" || menu[len(menu)-1].Command != "group"+strconv.Itoa(maxMenuCommands-1) {
		t.Errorf("menu = %d entries from %s to %s, want the first %d in order", len(menu), menu[0].Command, menu[len(menu)-1].Command, maxMenuCommands)
	}
	if got := menuCommands(nil, command.AudienceOwner); len(got) != 0 {
		t.Errorf("an empty binding menu = %v", got)
	}
}

func TestRelayedMessagesAreNeverRunAsCommands(t *testing.T) {
	forwarded := json.RawMessage(`{"type":"user","date":1700000000,"sender_user":{"id":777,"is_bot":false,"first_name":"x"}}`)
	tests := []struct {
		name string
		text string
		edit func(*message)
	}{
		{"a forwarded operation command", `/cron add --name x --schedule "*/5 * * * *" --channel telegram --destination me --prompt "do it"`, func(m *message) { m.ForwardOrigin = forwarded }},
		{"a forwarded delete", "/cron delete job1", func(m *message) { m.ForwardOrigin = forwarded }},
		{"a forwarded new session command", "/new", func(m *message) { m.ForwardOrigin = forwarded }},
		{"a forwarded skill command", "/skill review the diff", func(m *message) { m.ForwardOrigin = forwarded }},
		{"a forwarded forget command", "/forgetme confirm", func(m *message) { m.ForwardOrigin = forwarded }},
		{"a legacy forwarded command", "/new", func(m *message) { m.ForwardDate = 1700000000 }},
		{"a command sent through an inline bot", "/cron list", func(m *message) { m.ViaBot = &user{ID: 888, IsBot: true} }},
		{"an automatic forward", "/new", func(m *message) { m.AutomaticForward = true }},
		{"a forwarded caption", "/cron list", func(m *message) {
			m.ForwardOrigin = forwarded
			m.Caption, m.Text = m.Text, ""
		}},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spy := &operationSpy{}
			rig := newOperationCommandRig(t, accessOpts{}, spy)
			item := privateText(int64(100+i), ownerUserID, tt.text)
			tt.edit(item.Message)

			rig.handle(t, item)

			if planned, ran := spy.counts(); planned != 0 || ran != 0 {
				t.Errorf("planned %d ran %d for a relayed command", planned, ran)
			}
			if created := rig.sessions.createdSessions(); len(created) != 0 {
				t.Errorf("created sessions %v for a relayed command", created)
			}
			if replies := sentTo(rig.h.api, ownerUserID); len(replies) != 0 {
				t.Errorf("replies = %q, want none", replies)
			}
			if accepted := rig.sink.accepted(); len(accepted) != 1 || accepted[0].Parts[0].Text != tt.text || accepted[0].Skill != nil {
				t.Errorf("accepted = %+v, want the text handed to the model unchanged and unpinned", accepted)
			}
		})
	}
	t.Run("a typed command still runs", func(t *testing.T) {
		spy := &operationSpy{}
		rig := newOperationCommandRig(t, accessOpts{}, spy)
		rig.handle(t, privateText(200, ownerUserID, "/cron list"))
		if _, ran := spy.counts(); ran != 1 {
			t.Errorf("ran %d, want the typed command to run once", ran)
		}
	})
}
