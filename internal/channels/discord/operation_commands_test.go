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
