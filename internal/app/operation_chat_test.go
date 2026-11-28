package app

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/channels/kit/chatcommand"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
)

type chatRigE2E struct {
	*registryRig
	dispatcher *command.Dispatcher
	messages   int
}

func newChatRigE2E(t *testing.T) *chatRigE2E {
	t.Helper()
	rig := newRegistryRig(t, nil)
	chat, err := operation.NewChat(&operation.ChatDeps{Registry: rig.Registry, Capabilities: capabilitytest.Host(t)})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	operations := &deferredCommandOperations{}
	if err := operations.bind(chat); err != nil {
		t.Fatalf("bind: %v", err)
	}
	dispatcher, err := newChatCommandDispatcher(&chatCommandDeps{turns: &deferredCommandTurns{}, skills: &deferredCommandSkills{}, operations: operations, recorder: wireRecorder{}})
	if err != nil {
		t.Fatalf("newChatCommandDispatcher: %v", err)
	}
	return &chatRigE2E{registryRig: rig, dispatcher: dispatcher}
}

type chatSender struct {
	channel string
	class   principal.Class
	direct  bool
}

func (r *chatRigE2E) say(t *testing.T, sender chatSender, text string) []string {
	t.Helper()
	r.messages++
	inv, ok := command.Parse(text)
	if !ok {
		t.Fatalf("Parse(%q) is not a command", text)
	}
	caller := &command.Caller{
		Channel: sender.channel, PrincipalID: sender.channel + ":" + string(sender.class), Class: sender.class,
		ConversationKey: "conv", SessionID: operationtest.SessionID, AgentID: "main", Direct: sender.direct,
	}
	outcome, err := chatcommand.Execute(t.Context(), &chatcommand.Run{
		Effects: r.Executor, Dispatcher: r.dispatcher, Logger: slog.New(slog.DiscardHandler),
		Caller: caller, Invocation: &inv, IdempotencyKey: fmt.Sprintf("%s:%d", sender.channel, r.messages), EffectSession: operationtest.SessionID,
	})
	if err != nil {
		t.Fatalf("Execute(%q): %v", text, err)
	}
	if outcome.Receipt == nil {
		t.Fatalf("Execute(%q) replied nothing", text)
	}
	return outcome.Receipt.Lines
}

func (r *chatRigE2E) jobs(t *testing.T) string {
	t.Helper()
	var jobs string
	if err := r.DB.QueryRowContext(t.Context(), `SELECT COALESCE(group_concat(id || ':' || state), '') FROM (SELECT id, state FROM scheduled_job ORDER BY id)`).Scan(&jobs); err != nil {
		t.Fatalf("read jobs: %v", err)
	}
	return jobs
}

func TestOperationChatCommandsOnlyInDirectChats(t *testing.T) {
	for _, channel := range []string{principal.ChannelTelegram, principal.ChannelDiscord} {
		owner := chatSender{channel: channel, class: principal.ClassOwner, direct: true}
		t.Run(channel+" owner in a direct chat", func(t *testing.T) {
			rig := newChatRigE2E(t)

			added := rig.say(t, owner, `/cron add --name nightly --schedule "0 21 * * *" --timezone UTC --prompt "summarize the day" --channel telegram --destination me`)
			id, found := strings.CutPrefix(added[0], "id: ")
			if !found || len(added) != 1 {
				t.Fatalf("add replied %q, want the new job id", added)
			}
			listed := rig.say(t, owner, "/cron list")
			if len(listed) != 2 || !strings.HasPrefix(listed[0], "ID\tNAME\t") || !strings.Contains(listed[1], id+"\tnightly\t") {
				t.Fatalf("list replied %q, want the table with the job", listed)
			}
			paused := rig.say(t, owner, "/cron pause "+id)
			if !slices.Equal(paused, []string{"id: " + id, "state: paused"}) || rig.jobs(t) != id+":paused" {
				t.Fatalf("pause replied %q with jobs %q, want the paused job", paused, rig.jobs(t))
			}
		})
		t.Run(channel+" refuses a group or guild channel", func(t *testing.T) {
			rig := newChatRigE2E(t)
			direct := rig.say(t, owner, `/cron add --name nightly --schedule "0 21 * * *" --timezone UTC --prompt "summarize the day" --channel telegram --destination me`)
			id := strings.TrimPrefix(direct[0], "id: ")
			before := rig.jobs(t)

			for _, text := range []string{"/cron list", "/cron pause " + id, "/cron nonsense"} {
				got := rig.say(t, chatSender{channel: channel, class: principal.ClassOwner}, text)
				if !slices.Equal(got, []string{command.ReplyDirectOnly}) {
					t.Errorf("%q outside a direct chat replied %q, want only the scope refusal", text, got)
				}
			}
			if rig.jobs(t) != before {
				t.Errorf("jobs changed from %q to %q", before, rig.jobs(t))
			}
		})
		t.Run(channel+" refuses other principals in a direct chat", func(t *testing.T) {
			rig := newChatRigE2E(t)
			direct := rig.say(t, owner, `/cron add --name nightly --schedule "0 21 * * *" --timezone UTC --prompt "summarize the day" --channel telegram --destination me`)
			id := strings.TrimPrefix(direct[0], "id: ")
			before := rig.jobs(t)

			for _, class := range []principal.Class{principal.ClassTrusted, principal.ClassPublic} {
				for _, text := range []string{"/cron list", "/cron pause " + id} {
					got := rig.say(t, chatSender{channel: channel, class: class, direct: true}, text)
					if !slices.Equal(got, []string{command.ReplyDenied}) {
						t.Errorf("%q from a %s principal replied %q, want only the denial", text, class, got)
					}
				}
			}
			if rig.jobs(t) != before {
				t.Errorf("jobs changed from %q to %q", before, rig.jobs(t))
			}
		})
	}
}

func TestChatOperationCommandsAreAnsweredOnceWhenAMessageIsRedelivered(t *testing.T) {
	rig := newChatRigE2E(t)
	owner := chatSender{channel: principal.ChannelTelegram, class: principal.ClassOwner, direct: true}
	added := rig.say(t, owner, `/cron add --name nightly --schedule "0 21 * * *" --timezone UTC --prompt "summarize the day" --channel telegram --destination me`)
	id := strings.TrimPrefix(added[0], "id: ")
	rig.messages = 40

	first := rig.say(t, owner, "/cron pause "+id)
	rig.messages = 40
	second := rig.say(t, owner, "/cron pause "+id)

	if !slices.Equal(first, second) || rig.jobs(t) != id+":paused" {
		t.Fatalf("first %q second %q jobs %q, want the same journaled reply for the redelivered message", first, second, rig.jobs(t))
	}
}
