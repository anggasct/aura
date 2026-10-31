package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/channel/discord"
	"github.com/anggasct/aura/internal/channel/terminal"
	"github.com/anggasct/aura/internal/command"
	"github.com/anggasct/aura/internal/principal"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
	"github.com/anggasct/aura/internal/store"
)

type parityPorts struct{}

func (parityPorts) EventCount(context.Context, string) (int, error) { return 12, nil }

func (parityPorts) Create(_ context.Context, caller *command.Caller) (command.Session, error) {
	return command.Session{ID: caller.SessionID + "~next", OwnerID: caller.PrincipalID, AgentID: caller.AgentID}, nil
}

func (parityPorts) Switch(_ context.Context, caller *command.Caller, id string) (command.Session, error) {
	return command.Session{ID: id, OwnerID: caller.PrincipalID, AgentID: caller.AgentID}, nil
}

func (parityPorts) CancelActive(context.Context, string) (turnID string, cancelled bool, err error) {
	return "turn-7", true, nil
}

func (parityPorts) Activity(string) (activeTurnID string, queued int) { return "turn-7", 1 }

func TestCommandParityAcrossTerminalAndDiscord(t *testing.T) {
	registry, err := command.New(terminal.CommandBinding(), discord.CommandBinding())
	if err != nil {
		t.Fatalf("command.New: %v", err)
	}
	ports := parityPorts{}
	dispatcher, err := command.NewDispatcher(registry, command.Ports{Sessions: ports, Turns: ports}, nil)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	callers := map[string]*command.Caller{
		"terminal":       {Channel: command.ChannelTerminal, PrincipalID: principal.OwnerID, Class: principal.ClassOwner, SessionID: "sess-parity", AgentID: "main"},
		"discord dm":     {Channel: principal.ChannelDiscord, PrincipalID: principal.OwnerID, Class: principal.ClassOwner, ConversationKey: "sess-parity", SessionID: "sess-parity", AgentID: "main"},
		"discord thread": {Channel: principal.ChannelDiscord, PrincipalID: principal.OwnerID, Class: principal.ClassOwner, ConversationKey: "sess-parity", SessionID: "sess-parity", AgentID: "main"},
	}
	for _, text := range []string{"/status", "/session", "/cancel", "/session sess-parity"} {
		results := map[string][]string{}
		for name, caller := range callers {
			inv, ok := command.Parse(text)
			if !ok {
				t.Fatalf("Parse(%q)", text)
			}
			result := dispatcher.Execute(t.Context(), caller, &inv)
			if result.Code != command.ErrorCodeOK {
				t.Fatalf("%s %s = %+v", name, text, result)
			}
			results[name] = result.Lines
		}
		if !slices.Equal(results["terminal"], results["discord dm"]) || !slices.Equal(results["terminal"], results["discord thread"]) {
			t.Errorf("%s results diverge: %q", text, results)
		}
	}
	rows := func(channel string) map[string]string {
		out := map[string]string{}
		for _, line := range registry.Help(channel, principal.ClassOwner)[1:] {
			fields := strings.Fields(line)
			out[fields[0]] = strings.Join(fields, " ")
		}
		return out
	}
	terminalRows, discordRows := rows(command.ChannelTerminal), rows(principal.ChannelDiscord)
	for name, row := range discordRows {
		if terminalRows[name] != row {
			t.Errorf("help row %s: terminal %q, discord %q", name, terminalRows[name], row)
		}
	}
}

type stubActivity struct {
	cancelled []string
}

func (s *stubActivity) CancelActive(_ context.Context, sessionID string) (turnID string, cancelled bool, err error) {
	s.cancelled = append(s.cancelled, sessionID)
	return "turn-1", true, nil
}

func (s *stubActivity) SessionActivity(string) (activeTurnID string, queued int) { return "turn-1", 3 }

func TestDiscordCommandSessionsAgainstTheStore(t *testing.T) {
	db, err := store.OpenDB(t.Context(), filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	sessions := store.NewSessionService(db)
	now := time.Now().UTC()
	for _, sess := range []store.Session{
		{ID: "main/discord:owner/dm:111", OwnerID: principal.OwnerID, AgentID: "main"},
		{ID: "main/discord:owner/dm:222", OwnerID: "discord:222", AgentID: "main"},
	} {
		sess.Metadata, sess.CreatedAt, sess.UpdatedAt = json.RawMessage(`{}`), now, now
		if err := sessions.Create(t.Context(), &sess); err != nil {
			t.Fatalf("create session: %v", err)
		}
	}
	events := store.NewEventStore(db)
	for range 2 {
		if _, err := events.AppendSequenced(t.Context(), "main/discord:owner/dm:111", &store.RuntimeEvent{ID: "ev-" + time.Now().Format(time.RFC3339Nano), SessionID: "main/discord:owner/dm:111", Kind: "message.completed", SchemaVersion: 1, Payload: json.RawMessage(`{}`), CreatedAt: now}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	port := &discordCommandSessions{sessions: sessions, events: events}
	if n, err := port.EventCount(t.Context(), "main/discord:owner/dm:111"); err != nil || n != 2 {
		t.Errorf("EventCount = %d %v, want 2", n, err)
	}
	caller := &command.Caller{Channel: principal.ChannelDiscord, PrincipalID: principal.OwnerID, Class: principal.ClassOwner, ConversationKey: "main/discord:owner/dm:111", SessionID: "main/discord:owner/dm:111", AgentID: "main"}
	if sess, err := port.Switch(t.Context(), caller, "main/discord:owner/dm:111"); err != nil || sess.ID != caller.ConversationKey {
		t.Errorf("Switch to own conversation = %+v %v", sess, err)
	}
	for _, id := range []string{"main/discord:owner/dm:222", "sess_terminal"} {
		if _, err := port.Switch(t.Context(), caller, id); !isNotSwitchable(err) {
			t.Errorf("Switch(%s) error = %v, want not switchable", id, err)
		}
	}
	other := *caller
	other.ConversationKey, other.AgentID = "main/discord:owner/dm:222", "main"
	if _, err := port.Switch(t.Context(), &other, "main/discord:owner/dm:222"); !isNotSwitchable(err) {
		t.Errorf("Switch to a foreign-owned session error = %v", err)
	}
}

func isNotSwitchable(err error) bool {
	code, ok := command.CodeOf(err)
	return ok && code == command.ErrorCodeNotSwitchable
}

func TestDiscordCommandWiringDelegatesToTheEngine(t *testing.T) {
	activity := &stubActivity{}
	recorder := &wireRecorder{}
	if err := wireDiscordCommands([]runtimechannelhost.ChannelPort{}, activity, nil, nil, recorder); err != nil {
		t.Fatalf("wire without adapters: %v", err)
	}
	turns := engineCommandTurns{engine: activity}
	if id, cancelled, err := turns.CancelActive(t.Context(), "s"); err != nil || !cancelled || id != "turn-1" || !slices.Equal(activity.cancelled, []string{"s"}) {
		t.Errorf("CancelActive = %q %v %v", id, cancelled, err)
	}
	if active, queued := turns.Activity("s"); active != "turn-1" || queued != 3 {
		t.Errorf("Activity = %q %d", active, queued)
	}
}

type wireRecorder struct{}

func (wireRecorder) Start(ctx context.Context, _, _, _ string) (spanCtx context.Context, finish func(string)) {
	return ctx, func(string) {}
}
