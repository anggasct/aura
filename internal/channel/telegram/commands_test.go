package telegram

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/channel/access"
	"github.com/anggasct/aura/internal/command"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
)

type commandSessions struct {
	mu      sync.Mutex
	created []string
}

func (s *commandSessions) EventCount(context.Context, string) (int, error) {
	return 3, nil
}

func (s *commandSessions) Create(_ context.Context, caller *command.Caller) (command.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := caller.ConversationKey + "~fresh"
	s.created = append(s.created, id)
	return command.Session{ID: id, OwnerID: caller.PrincipalID, AgentID: caller.AgentID}, nil
}

func (s *commandSessions) Switch(context.Context, *command.Caller, string) (command.Session, error) {
	return command.Session{}, errors.New("unused")
}

func (s *commandSessions) createdSessions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.created)
}

type pinningSkills struct{}

func (pinningSkills) Activate(_ context.Context, name string) (command.SkillPin, error) {
	return command.SkillPin{SkillID: name, Digest: "digest-1"}, nil
}

func (pinningSkills) StageDraft(context.Context, string, string) (command.Draft, error) {
	return command.Draft{}, errors.New("unused")
}

type commandRig struct {
	*accessRig
	sessions *commandSessions
}

func newCommandRig(t *testing.T, opts accessOpts) *commandRig {
	t.Helper()
	rig := newAccessRig(t, opts)
	registry, err := command.New(CommandBinding())
	if err != nil {
		t.Fatalf("command.New: %v", err)
	}
	sessions := &commandSessions{}
	dispatcher, err := command.NewDispatcher(registry, command.Ports{Sessions: sessions, Skills: pinningSkills{}}, nil)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	rig.adapter.SetCommands(dispatcher)
	rig.adapter.sink = rig.sink
	return &commandRig{accessRig: rig, sessions: sessions}
}

func (r *commandRig) knowBot() {
	r.adapter.identity.Store(&botIdentity{ID: testBotID, Username: testBotName})
}

func (r *commandRig) handle(t *testing.T, item update) {
	t.Helper()
	if err := r.adapter.handleUpdate(t.Context(), &item); err != nil {
		t.Fatalf("handleUpdate: %v", err)
	}
}

func commandEntity(item update, token string) update {
	item.Message.Entities = []messageEntity{entityOf(item.Message.Text, token, entityBotCommand)}
	return item
}

func TestCommandsRunInAPrivateChat(t *testing.T) {
	rig := newCommandRig(t, accessOpts{})
	cases := []struct {
		name string
		text string
		want string
	}{
		{"help lists the command set", "/help", "commands:"},
		{"status shows the session", "/status", "session main/telegram:pocket/chat:" + strconv.Itoa(ownerUserID)},
		{"new starts a session", "/new", "new session main/telegram:pocket/chat:" + strconv.Itoa(ownerUserID) + "~fresh"},
		{"command word is case insensitive", "/Help", "commands:"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(sentTo(rig.h.api, ownerUserID))
			rig.handle(t, privateText(int64(10+i), ownerUserID, tc.text))
			rig.assertQuiet(t)
			replies := sentTo(rig.h.api, ownerUserID)
			if len(replies) != before+1 || !strings.Contains(replies[before], tc.want) {
				t.Fatalf("replies = %q, want one more containing %q", replies, tc.want)
			}
		})
	}
}

func TestHelpListsTheSharedCommandSet(t *testing.T) {
	rig := newCommandRig(t, accessOpts{})
	rig.handle(t, privateText(10, ownerUserID, "/help"))
	replies := sentTo(rig.h.api, ownerUserID)
	if len(replies) != 1 {
		t.Fatalf("replies = %q", replies)
	}
	for _, name := range []string{"/help", "/status", "/session", "/new", "/cancel", "/skill", "/skill-create"} {
		if !strings.Contains(replies[0], name) {
			t.Errorf("help does not list %s: %q", name, replies[0])
		}
	}
	if strings.Contains(replies[0], "/exit") || strings.Contains(replies[0], "/clear") {
		t.Errorf("help lists terminal commands: %q", replies[0])
	}
}

func TestNewCommandCreatesASessionPerConversation(t *testing.T) {
	rig := newCommandRig(t, accessOpts{})
	rig.handle(t, privateText(10, ownerUserID, "/new"))
	want := "main/telegram:pocket/chat:" + strconv.Itoa(ownerUserID) + "~fresh"
	if got := rig.sessions.createdSessions(); !slices.Equal(got, []string{want}) {
		t.Errorf("created sessions = %v, want %v", got, want)
	}
}

func TestCommandsFollowTheCallerClass(t *testing.T) {
	rig := newCommandRig(t, accessOpts{allowed: []string{strconv.Itoa(allowedUserID)}})
	rig.handle(t, privateText(10, allowedUserID, "/status"))
	rig.handle(t, privateText(11, allowedUserID, "/help"))
	replies := sentTo(rig.h.api, allowedUserID)
	if len(replies) != 2 || replies[0] != command.ReplyDenied || !strings.Contains(replies[1], "commands:") {
		t.Fatalf("replies = %q, want a denial then the help", replies)
	}
	if strings.Contains(replies[1], "/status") {
		t.Errorf("a non-owner help lists owner commands: %q", replies[1])
	}
}

func TestCommandRepliesAreSentOnceWhenAnUpdateReplays(t *testing.T) {
	rig := newCommandRig(t, accessOpts{})
	item := privateText(10, ownerUserID, "/status")
	rig.handle(t, item)
	rig.handle(t, item)
	if replies := sentTo(rig.h.api, ownerUserID); len(replies) != 1 {
		t.Errorf("replies = %q, want the journaled reply sent once", replies)
	}
}

func TestSkillCommandContinuesAsATurnWithThePin(t *testing.T) {
	rig := newCommandRig(t, accessOpts{})
	rig.handle(t, privateText(10, ownerUserID, "/skill review the diff"))
	accepted := rig.sink.accepted()
	if len(accepted) != 1 {
		t.Fatalf("accepted %d envelopes, want 1", len(accepted))
	}
	pin := accepted[0].Skill
	if pin == nil || pin.SkillID != "review" || accepted[0].Parts[0].Text != "/skill review the diff" {
		t.Errorf("envelope = %+v, want the pinned skill and the original text", accepted[0])
	}
	if replies := sentTo(rig.h.api, ownerUserID); len(replies) != 0 {
		t.Errorf("replies = %q, want none", replies)
	}
}

func TestUnknownSlashTextGoesToTheModel(t *testing.T) {
	rig := newCommandRig(t, accessOpts{})
	rig.handle(t, privateText(10, ownerUserID, "/deploy now"))
	rig.handle(t, privateText(11, ownerUserID, "please /help"))
	if accepted := rig.sink.accepted(); len(accepted) != 2 {
		t.Fatalf("accepted %d envelopes, want 2", len(accepted))
	}
}

func TestStartStaysThePairingEntryAndIsNotACommand(t *testing.T) {
	t.Run("known sender text goes to the model", func(t *testing.T) {
		rig := newCommandRig(t, accessOpts{})
		rig.handle(t, privateText(10, ownerUserID, "/start"))
		if accepted := rig.sink.accepted(); len(accepted) != 1 || accepted[0].Parts[0].Text != "/start" {
			t.Fatalf("accepted = %+v, want /start forwarded as text", accepted)
		}
		if replies := sentTo(rig.h.api, ownerUserID); len(replies) != 0 {
			t.Errorf("replies = %q, want none", replies)
		}
	})
	t.Run("unknown sender is paired", func(t *testing.T) {
		pairing := &fakePairing{result: access.PairingResult{Outcome: access.PairingOutcomeIssued, Code: "ABCD2345"}}
		definition := auraagent.Definition{ID: testPersona, Audience: config.AudienceTrusted, Pairing: true}
		rig := newCommandRig(t, accessOpts{definition: definition, settings: AccessSettings{Pairing: pairing}})
		rig.handle(t, privateText(10, strangerID, "/start"))
		rig.assertQuiet(t, "pairing")
		want := strings.ReplaceAll(personaMessage(config.MessagePairingCode), config.PairingCodePlaceholder, "ABCD2345")
		if got := sentTo(rig.h.api, strangerID); !slices.Equal(got, []string{want}) {
			t.Errorf("pairing reply = %q, want %q", got, want)
		}
	})
}

func TestForgetMeBehavesTheSameThroughTheDispatcher(t *testing.T) {
	t.Run("confirmation erases the sender", func(t *testing.T) {
		eraser := &fakeEraser{}
		rig := newCommandRig(t, accessOpts{allowed: []string{strconv.Itoa(allowedUserID)}, settings: AccessSettings{Erasure: eraser}})
		rig.handle(t, privateText(10, allowedUserID, "/forgetme confirm"))
		rig.assertQuiet(t)
		if erased := eraser.erased(); !slices.Equal(erased, []string{"telegram:555001"}) {
			t.Errorf("erased %v", erased)
		}
		if got := sentTo(rig.h.api, allowedUserID); !slices.Equal(got, []string{personaMessage(config.MessageForgetmeDone)}) {
			t.Errorf("reply = %q", got)
		}
	})
	t.Run("the owner is not permitted", func(t *testing.T) {
		rig := newCommandRig(t, accessOpts{settings: AccessSettings{Erasure: &fakeEraser{}}})
		rig.handle(t, privateText(10, ownerUserID, "/forgetme confirm"))
		if got := sentTo(rig.h.api, ownerUserID); !slices.Equal(got, []string{personaMessage(config.MessageNotPermitted)}) {
			t.Errorf("reply = %q", got)
		}
	})
	t.Run("without an erasure service the text goes to the model", func(t *testing.T) {
		rig := newCommandRig(t, accessOpts{})
		rig.handle(t, privateText(10, ownerUserID, "/forgetme"))
		rig.onlyEnvelope(t)
		if replies := sentTo(rig.h.api, ownerUserID); len(replies) != 0 {
			t.Errorf("replies = %q, want none", replies)
		}
	})
}

func TestBotSuffixIsStrippedWhenTheBotIdentityIsKnown(t *testing.T) {
	handle := "@" + testBotName
	t.Run("private chat with a known identity", func(t *testing.T) {
		rig := newCommandRig(t, accessOpts{})
		rig.knowBot()
		rig.handle(t, privateText(10, ownerUserID, "/help"+handle))
		if replies := sentTo(rig.h.api, ownerUserID); len(replies) != 1 || !strings.Contains(replies[0], "commands:") {
			t.Fatalf("replies = %q, want the help", replies)
		}
	})
	t.Run("suffix match is case insensitive", func(t *testing.T) {
		rig := newCommandRig(t, accessOpts{})
		rig.knowBot()
		rig.handle(t, privateText(10, ownerUserID, "/status@AURA_Test_Bot"))
		if replies := sentTo(rig.h.api, ownerUserID); len(replies) != 1 {
			t.Fatalf("replies = %q, want one", replies)
		}
	})
	t.Run("another bot's suffix is not a command", func(t *testing.T) {
		rig := newCommandRig(t, accessOpts{})
		rig.knowBot()
		rig.handle(t, privateText(10, ownerUserID, "/help@"+otherBotName))
		rig.onlyEnvelope(t)
		if replies := sentTo(rig.h.api, ownerUserID); len(replies) != 0 {
			t.Errorf("replies = %q, want none", replies)
		}
	})
	t.Run("an unknown identity leaves the suffix alone", func(t *testing.T) {
		rig := newCommandRig(t, accessOpts{})
		rig.handle(t, privateText(10, ownerUserID, "/help"+handle))
		rig.onlyEnvelope(t)
	})
}

func TestGroupCommandsNeedTheBotSuffix(t *testing.T) {
	handle := "@" + testBotName
	t.Run("addressed command runs and replies in the group", func(t *testing.T) {
		rig := newCommandRig(t, accessOpts{chats: groupChats})
		rig.knowBot()
		rig.handle(t, commandEntity(groupMessage(10, ownerUserID, "/help"+handle), "/help"+handle))
		rig.assertQuiet(t)
		replies := sentTo(rig.h.api, groupChatID)
		if len(replies) != 1 || !strings.Contains(replies[0], "commands:") {
			t.Fatalf("group replies = %q, want the help", replies)
		}
	})
	t.Run("addressed command in a forum topic replies in the topic", func(t *testing.T) {
		rig := newCommandRig(t, accessOpts{chats: groupChats})
		rig.knowBot()
		rig.handle(t, inTopic(commandEntity(groupMessage(10, ownerUserID, "/status"+handle), "/status"+handle), forumTopic))
		sends := rig.h.api.sends()
		if len(sends) != 1 || sends[0]["message_thread_id"] != float64(forumTopic) || sends[0]["chat_id"] != float64(groupChatID) {
			t.Fatalf("sends = %v, want one reply inside the topic", sends)
		}
		if text, _ := sends[0]["text"].(string); !strings.Contains(text, "chat:"+strconv.Itoa(groupChatID)+"/topic:"+strconv.Itoa(forumTopic)) {
			t.Errorf("reply = %q, want the topic conversation", text)
		}
	})
	t.Run("a command for another bot is not addressed to this one", func(t *testing.T) {
		rig := newCommandRig(t, accessOpts{chats: groupChats})
		rig.knowBot()
		other := "/help@" + otherBotName
		rig.handle(t, commandEntity(groupMessage(10, ownerUserID, other), other))
		rig.assertQuiet(t, "not_mentioned")
		if sends := rig.h.api.sends(); len(sends) != 0 {
			t.Errorf("sends = %v, want none", sends)
		}
	})
	t.Run("another bot's command in an addressed message is plain text", func(t *testing.T) {
		rig := newCommandRig(t, accessOpts{chats: groupChats})
		rig.knowBot()
		rig.handle(t, mentioning(10, ownerUserID, handle+" /help@"+otherBotName, handle))
		if text := rig.onlyText(t); text != "/help@"+otherBotName {
			t.Errorf("model text = %q", text)
		}
		if sends := rig.h.api.sends(); len(sends) != 0 {
			t.Errorf("sends = %v, want none", sends)
		}
	})
	t.Run("a bare command in the group is not addressed", func(t *testing.T) {
		rig := newCommandRig(t, accessOpts{chats: groupChats})
		rig.knowBot()
		rig.handle(t, groupMessage(10, ownerUserID, "/help"))
		rig.assertQuiet(t, "not_mentioned")
	})
}

func TestGroupMembersCanRunTheHelpCommand(t *testing.T) {
	handle := "@" + testBotName
	rig := newCommandRig(t, accessOpts{chats: groupChats, allowed: []string{strconv.Itoa(allowedUserID)}})
	rig.knowBot()
	rig.handle(t, commandEntity(groupMessage(10, allowedUserID, "/help"+handle), "/help"+handle))
	replies := sentTo(rig.h.api, groupChatID)
	if len(replies) != 1 || !strings.Contains(replies[0], "commands:") {
		t.Fatalf("group replies = %q, want the help for the allowlisted member", replies)
	}
}

func TestCommandsRunBeforeBudgetsAndNeverBecomeTurns(t *testing.T) {
	budgets := &fakeBudgets{}
	rig := newCommandRig(t, accessOpts{allowed: []string{strconv.Itoa(allowedUserID)}, settings: AccessSettings{Budgets: budgets}})
	rig.handle(t, privateText(10, allowedUserID, "/help"))
	if calls := budgets.recorded(); len(calls) != 0 {
		t.Errorf("budget checks = %v, want none for a command", calls)
	}
	rig.assertQuiet(t)
}

func TestCommandsAreNotInterceptedWithoutADispatcher(t *testing.T) {
	rig := newAccessRig(t, accessOpts{})
	rig.adapter.sink = rig.sink
	if err := rig.adapter.handleUpdate(t.Context(), &update{UpdateID: 10, Message: privateText(10, ownerUserID, "/help").Message}); err != nil {
		t.Fatalf("handleUpdate: %v", err)
	}
	rig.onlyEnvelope(t)
}

func TestCommandBindingNamesTheSharedSetOnAMessengerChannel(t *testing.T) {
	binding := CommandBinding()
	if binding.Channel != principal.ChannelTelegram || binding.Style != command.StyleMessenger || !slices.Equal(binding.Names, command.MessengerNames()) {
		t.Errorf("binding = %+v", binding)
	}
}
