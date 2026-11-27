package telegram

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/principal"
)

const menuWarning = "telegram command menu not registered"

var telegramCommandName = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

type menuEntry struct {
	scope   string
	chatID  float64
	command []string
}

func menuCalls(t *testing.T, rig *commandRig) []menuEntry {
	t.Helper()
	var entries []menuEntry
	for _, body := range rig.h.api.bodies("setMyCommands") {
		scope, _ := body["scope"].(map[string]any)
		entry := menuEntry{}
		entry.scope, _ = scope["type"].(string)
		entry.chatID, _ = scope["chat_id"].(float64)
		items, _ := body["commands"].([]any)
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			name, _ := item["command"].(string)
			description, _ := item["description"].(string)
			if !telegramCommandName.MatchString(name) {
				t.Errorf("command %q is not a valid Telegram command name", name)
			}
			if n := utf8.RuneCountInString(description); n < 3 || n > 256 {
				t.Errorf("description of %q has %d characters, want 3 to 256", name, n)
			}
			entry.command = append(entry.command, name)
		}
		entries = append(entries, entry)
	}
	return entries
}

func startAndAwaitPoll(t *testing.T, rig *commandRig) {
	t.Helper()
	rig.h.api.hold = true
	stop := startAdapter(t, rig.adapter, rig.sink)
	defer stop()
	rig.h.api.awaitPoll(t)
}

func TestStartRegistersTheCommandMenuPerAudience(t *testing.T) {
	rig := newCommandRig(t, accessOpts{})
	startAndAwaitPoll(t, rig)
	want := []menuEntry{
		{scope: "default", command: []string{"help", "forgetme"}},
		{scope: "chat", chatID: ownerUserID, command: []string{"help", "status", "session", "new", "cancel", "skill", "skill_create"}},
	}
	got := menuCalls(t, rig)
	if len(got) != len(want) {
		t.Fatalf("setMyCommands calls = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].scope != want[i].scope || got[i].chatID != want[i].chatID || !slices.Equal(got[i].command, want[i].command) {
			t.Errorf("setMyCommands call %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestSkillCreateUnderscoreNameIsAcceptedOnTelegram(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"telegram name", "/skill_create a b"},
		{"telegram name is case insensitive", "/Skill_Create a b"},
		{"telegram name with the bot suffix", "/skill_create@" + testBotName + " a b"},
		{"hand typed catalog name", "/skill-create a b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			skills := &stagingSkills{}
			registry, err := command.New(CommandBinding())
			if err != nil {
				t.Fatalf("command.New: %v", err)
			}
			dispatcher, err := command.NewDispatcher(registry, command.Ports{Sessions: &commandSessions{}, Skills: skills}, nil)
			if err != nil {
				t.Fatalf("NewDispatcher: %v", err)
			}
			rig := newAccessRig(t, accessOpts{commands: dispatcher})
			rig.adapter.sink = rig.sink
			rig.adapter.identity.Store(&botIdentity{ID: testBotID, Username: testBotName})
			item := privateText(10, ownerUserID, tc.text)
			if err := rig.adapter.handleUpdate(t.Context(), &item); err != nil {
				t.Fatalf("handleUpdate: %v", err)
			}
			if got := skills.staged(); !slices.Equal(got, []string{"a:b"}) {
				t.Errorf("staged drafts = %v, want [a:b]", got)
			}
			rig.assertQuiet(t)
		})
	}
}

func TestCommandMenuFailureDoesNotStopTheAdapter(t *testing.T) {
	t.Run("a rejected call logs one warning and polling starts", func(t *testing.T) {
		rig := newCommandRig(t, accessOpts{})
		rig.h.api.replies = append(rig.h.api.replies, scriptedReply{method: "setMyCommands", reply: apiFailure(400, 0)})
		startAndAwaitPoll(t, rig)
		if warnings := strings.Count(rig.h.logs.String(), menuWarning); warnings != 1 {
			t.Errorf("menu warnings = %d, want 1: %s", warnings, rig.h.logs.String())
		}
		if got := rig.h.api.count("setMyCommands"); got != 2 {
			t.Errorf("setMyCommands calls = %d, want both scopes attempted", got)
		}
	})
	t.Run("without an owner identity only the default scope is sent", func(t *testing.T) {
		owners, err := principal.NewResolver([]string{"discord:777"})
		if err != nil {
			t.Fatalf("NewResolver: %v", err)
		}
		rig := newCommandRig(t, accessOpts{owners: owners})
		startAndAwaitPoll(t, rig)
		got := menuCalls(t, rig)
		if len(got) != 1 || got[0].scope != "default" || !slices.Equal(got[0].command, []string{"help", "forgetme"}) {
			t.Errorf("setMyCommands calls = %+v, want only the default scope", got)
		}
		if strings.Contains(rig.h.logs.String(), menuWarning) {
			t.Errorf("logged a menu warning without a failure: %s", rig.h.logs.String())
		}
	})
}

type stagingSkills struct {
	pinningSkills
	mu     sync.Mutex
	drafts []string
}

func (s *stagingSkills) StageDraft(_ context.Context, name, description string) (command.Draft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drafts = append(s.drafts, name+":"+description)
	return command.Draft{ID: "draft-1", Digest: "digest-1"}, nil
}

func (s *stagingSkills) staged() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.drafts)
}
