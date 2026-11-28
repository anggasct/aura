package discord

import (
	"context"
	"encoding/json"
	"slices"
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
	input, _ := json.Marshal(map[string][]string{"tokens": tokens})
	return command.OperationPlan{Operation: "cron.list", Input: input}
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

func (f *commandFixture) withOperations(t *testing.T, spy *operationSpy) {
	t.Helper()
	binding := CommandBinding()
	binding.Names = append(binding.Names, "cron")
	group := command.Definition{Name: "cron", Usage: "<verb> [arguments]", Summary: "Manage scheduled recurring jobs", Audience: command.AudienceOwner, Kind: command.KindOperation, MinArgs: 1, MaxArgs: command.Variadic}
	registry, err := command.NewRegistry(append(command.Catalog(), group), binding)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	dispatcher, err := command.NewDispatcher(registry, command.Ports{Sessions: f.ports, Turns: f.ports, Skills: f.ports, Operations: spy}, f.recorder)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	f.adapter.commands = dispatcher
}

func TestOperationCommandsRunOnlyInADirectMessageWithTheOwner(t *testing.T) {
	spy := &operationSpy{}
	f := newCommandFixture(t)
	f.withOperations(t, spy)

	f.admit(t, dmMessage("8001", "111", `/cron list "a b"`))
	replies := f.replies(t)
	if len(replies) != 1 || replies[0].channel != "555" || replies[0].content != "ID\tNAME\nj1\tnightly" {
		t.Fatalf("dm replies = %+v, want the operation rows", replies)
	}
	planned, ran := spy.counts()
	if ran != 1 || !slices.Equal(spy.planned[0], []string{"list", "a b"}) || !spy.callers[0].Direct {
		t.Fatalf("planned %d ran %d callers %+v, want one direct run", planned, ran, spy.callers)
	}

	f.admit(t, threadTrigger("9100", "<@999> /cron list", []string{"999"}))
	replies = f.replies(t)
	if len(replies) != 2 || replies[1].channel != "333" || replies[1].content != command.ReplyDirectOnly {
		t.Fatalf("guild replies = %+v, want only the scope refusal in the channel", replies)
	}
	if again, ranAgain := spy.counts(); again != planned || ranAgain != ran {
		t.Errorf("after the guild message planned %d ran %d, want no further work than %d and %d", again, ranAgain, planned, ran)
	}
}

func TestOperationCommandsAreDeniedToOtherPrincipalsWithoutTouchingOperations(t *testing.T) {
	spy := &operationSpy{}
	f := newCommandFixture(t)
	f.withOperations(t, spy)

	f.admit(t, dmMessage("8002", "112", "/cron list"))

	replies := f.replies(t)
	if len(replies) != 1 || replies[0].content != command.ReplyDenied {
		t.Fatalf("replies = %+v, want only the denial", replies)
	}
	if planned, ran := spy.counts(); planned != 0 || ran != 0 {
		t.Errorf("planned %d ran %d for a trusted principal, want none", planned, ran)
	}
}

func TestOperationCommandsAreListedInHelpOnlyForTheOwnerInADirectMessage(t *testing.T) {
	f := newCommandFixture(t)
	f.withOperations(t, &operationSpy{})

	f.admit(t, dmMessage("8003", "111", "/help"))
	f.admit(t, dmMessage("8004", "112", "/help"))
	f.admit(t, threadTrigger("9101", "<@999> /help", []string{"999"}))

	replies := f.replies(t)
	if len(replies) != 3 {
		t.Fatalf("replies = %+v", replies)
	}
	if !strings.Contains(replies[0].content, "/cron <verb> [arguments]") || !strings.Contains(replies[0].content, "Manage scheduled recurring jobs") {
		t.Errorf("owner dm help = %q, want the cron group", replies[0].content)
	}
	for i, name := range map[int]string{1: "trusted dm", 2: "guild channel"} {
		if strings.Contains(replies[i].content, "/cron") {
			t.Errorf("%s help lists the operation group: %q", name, replies[i].content)
		}
	}
}

func TestRelayedAndApplicationMessagesAreNeverRunAsCommands(t *testing.T) {
	tests := []struct {
		name string
		edit func(*messagePayload)
	}{
		{"a forward with its content empty", func(m *messagePayload) {
			m.Content = ""
			m.MessageReference = &messageReferencePayload{Type: referenceTypeForward, MessageID: "7", ChannelID: "9"}
		}},
		{"a forward that carries command text", func(m *messagePayload) {
			m.MessageReference = &messageReferencePayload{Type: referenceTypeForward, MessageID: "7", ChannelID: "9"}
		}},
		{"an application command message", func(m *messagePayload) { m.Type = 20 }},
		{"a context menu command message", func(m *messagePayload) { m.Type = 23 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spy := &operationSpy{}
			f := newCommandFixture(t)
			f.withOperations(t, spy)
			msg := dmMessage("8101", "111", "/cron list")
			tt.edit(msg)

			if _, err := f.adapter.admit(t.Context(), msg); err != nil {
				t.Fatalf("admit: %v", err)
			}

			if planned, ran := spy.counts(); planned != 0 || ran != 0 {
				t.Errorf("planned %d ran %d for a relayed message", planned, ran)
			}
			if replies := f.replies(t); len(replies) != 0 {
				t.Errorf("replies = %+v, want none", replies)
			}
		})
	}
	t.Run("a reply with ordinary text goes to the model and runs nothing", func(t *testing.T) {
		spy := &operationSpy{}
		f := newCommandFixture(t)
		f.withOperations(t, spy)
		msg := dmMessage("8102", "111", "what does that do?")
		msg.Type = 19
		msg.MessageReference = &messageReferencePayload{MessageID: "7", ChannelID: "555"}

		f.admit(t, msg)

		if planned, ran := spy.counts(); planned != 0 || ran != 0 {
			t.Errorf("planned %d ran %d for a reply", planned, ran)
		}
		if calls, _ := f.sink.stats("message:8102"); calls != 1 {
			t.Errorf("the reply reached the model %d times, want once", calls)
		}
	})
	t.Run("a typed reply that is a command still runs", func(t *testing.T) {
		spy := &operationSpy{}
		f := newCommandFixture(t)
		f.withOperations(t, spy)
		msg := dmMessage("8103", "111", "/cron list")
		msg.Type = 19
		msg.MessageReference = &messageReferencePayload{MessageID: "7", ChannelID: "555"}

		f.admit(t, msg)

		if _, ran := spy.counts(); ran != 1 {
			t.Errorf("ran %d, want the typed command to run once", ran)
		}
	})
}

func TestAConflictingRedeliveryIsAnsweredAndAcknowledgedWithoutRunningAnything(t *testing.T) {
	spy := &operationSpy{}
	f := newCommandFixture(t)
	f.withOperations(t, spy)

	f.admit(t, dmMessage("8201", "111", "/cron list a"))
	f.admit(t, dmMessage("8201", "111", "/cron list b"))

	replies := f.replies(t)
	if len(replies) != 2 || replies[0].content != "ID\tNAME\nj1\tnightly" || replies[1].content != command.ReplyConflict {
		t.Fatalf("replies = %+v, want the first answer and the conflict reply", replies)
	}
	if _, ran := spy.counts(); ran != 1 {
		t.Errorf("ran %d, want only the first message", ran)
	}
	if got := strings.Count(f.logs.String(), "chat command conflicts with its earlier request"); got != 1 {
		t.Errorf("conflict warnings = %d, want one", got)
	}
	if calls, _ := f.sink.stats("message:8201"); calls != 0 {
		t.Errorf("the conflicting message reached the model %d times", calls)
	}
}
