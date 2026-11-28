package app

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/channels/discord"
	"github.com/anggasct/aura/internal/channels/kit/chatcommand"
	"github.com/anggasct/aura/internal/channels/telegram"
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
	if runs := rig.handlers.handled("cron.pause"); runs != 1 {
		t.Errorf("the pause handler ran %d times for one message, want once", runs)
	}
}

func TestDeferredCommandOperationsFailClosedUntilBoundAndBindOnce(t *testing.T) {
	deferred := &deferredCommandOperations{}
	plan := deferred.Plan("cron", []string{"list"})
	if plan.Refusal == nil || plan.Refusal.Code != command.ErrorCodeUnavailable || plan.Operation != "" {
		t.Fatalf("unbound Plan = %+v, want an unavailable refusal", plan)
	}
	reply := deferred.Run(t.Context(), &command.OperationRun{})
	if reply.Code != command.ErrorCodeUnavailable || !slices.Equal(reply.Lines, []string{command.ReplyOperationsUnavailable}) {
		t.Fatalf("unbound Run = %+v, want unavailable", reply)
	}
	rig := newRegistryRig(t, nil)
	chat, err := operation.NewChat(&operation.ChatDeps{Registry: rig.Registry})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	if err := deferred.bind(chat); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := deferred.bind(chat); err == nil {
		t.Error("a second bind was accepted")
	}
	if err := deferred.bind(nil); err == nil {
		t.Error("binding nothing was accepted")
	}
	if plan := deferred.Plan("cron", []string{"list"}); plan.Refusal != nil || plan.Operation != "cron.list" {
		t.Errorf("bound Plan = %+v, want the list operation", plan)
	}
}

func TestChatCommandRegistryListsEveryOperationGroupOnBothMessengers(t *testing.T) {
	dispatcher, err := newChatCommandDispatcher(&chatCommandDeps{turns: &deferredCommandTurns{}, skills: &deferredCommandSkills{}, operations: &deferredCommandOperations{}, recorder: wireRecorder{}})
	if err != nil {
		t.Fatalf("newChatCommandDispatcher: %v", err)
	}
	groups := operation.ChatGroups(operationCatalogs())
	if len(groups) == 0 {
		t.Fatal("the shipped catalogs project no chat group")
	}
	for _, channel := range []string{principal.ChannelTelegram, principal.ChannelDiscord} {
		bound := dispatcher.Registry().Definitions(channel)
		for i := range groups {
			if !slices.ContainsFunc(bound, func(d command.Definition) bool { return d.Name == groups[i].Name && d.Kind == command.KindOperation }) {
				t.Errorf("%s does not bind the %s group", channel, groups[i].Name)
			}
		}
	}
	if _, err := newChatCommandDispatcher(nil); err == nil {
		t.Error("a dispatcher was built without its ports")
	}
}

func TestChatCommandsAreNamedOnlyWhereTheOwnerCanUseThem(t *testing.T) {
	tests := []struct {
		name     string
		identity []string
		telegram *telegram.Config
		discord  *discord.Config
		want     bool
	}{
		{"nothing enabled", []string{"telegram:1", "discord:2"}, nil, nil, false},
		{"telegram with an owner identity", []string{"telegram:1"}, &telegram.Config{Enabled: true, Accounts: []telegram.Account{{Name: "main"}}}, nil, true},
		{"telegram without an owner identity", []string{"discord:2"}, &telegram.Config{Enabled: true, Accounts: []telegram.Account{{Name: "main"}}}, nil, false},
		{"telegram without accounts", []string{"telegram:1"}, &telegram.Config{Enabled: true}, nil, false},
		{"telegram disabled", []string{"telegram:1"}, &telegram.Config{Accounts: []telegram.Account{{Name: "main"}}}, nil, false},
		{"discord accepting direct messages", []string{"discord:2"}, nil, &discord.Config{Enabled: true, Accounts: []discord.Account{{Name: "main", AcceptDMs: true}}}, true},
		{"discord refusing direct messages", []string{"discord:2"}, nil, &discord.Config{Enabled: true, Accounts: []discord.Account{{Name: "main"}}}, false},
		{"discord without an owner identity", []string{"telegram:1"}, nil, &discord.Config{Enabled: true, Accounts: []discord.Account{{Name: "main", AcceptDMs: true}}}, false},
		{"one account accepting direct messages among others", []string{"discord:2"}, nil, &discord.Config{Enabled: true, Accounts: []discord.Account{{Name: "a"}, {Name: "b", AcceptDMs: true}}}, true},
		{"discord refusing but telegram reachable", []string{"telegram:1", "discord:2"}, &telegram.Config{Enabled: true, Accounts: []telegram.Account{{Name: "main"}}}, &discord.Config{Enabled: true, Accounts: []discord.Account{{Name: "main"}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := appconfig.Default()
			cfg.Owner = &principal.Owner{Identities: tt.identity}
			if tt.telegram != nil {
				cfg.Channels.Telegram = *tt.telegram
			}
			if tt.discord != nil {
				cfg.Channels.Discord = *tt.discord
			}
			owners, err := ownerResolver(&cfg)
			if err != nil {
				t.Fatalf("ownerResolver: %v", err)
			}
			if got := ownerReachableInChat(&cfg, owners); got != tt.want {
				t.Errorf("ownerReachableInChat = %t, want %t", got, tt.want)
			}
		})
	}
	if ownerReachableInChat(nil, nil) {
		t.Error("a missing config reported a reachable owner")
	}
}
