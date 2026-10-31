package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
	for _, text := range []string{"/status", "/session", "/new", "/cancel", "/session sess-parity"} {
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

func newCommandStore(t *testing.T) (store.SessionService, store.EventStore, store.ConversationSessionStore, *sql.DB) {
	t.Helper()
	db, err := store.OpenDB(t.Context(), filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return store.NewSessionService(db), store.NewEventStore(db), store.NewConversationSessionStore(db), db
}

func seedCommandSession(t *testing.T, sessions store.SessionService, id, owner, agent string) {
	t.Helper()
	now := time.Now().UTC()
	sess := store.Session{ID: id, OwnerID: owner, AgentID: agent, Metadata: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now}
	if err := sessions.Create(t.Context(), &sess); err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
}

func TestDiscordCommandSessionsAgainstTheStore(t *testing.T) {
	sessions, events, pointers, db := newCommandStore(t)
	key := "main/discord:owner/dm:111"
	seedCommandSession(t, sessions, key, principal.OwnerID, "main")
	seedCommandSession(t, sessions, "main/discord:owner/dm:222", "discord:222", "main")
	seedCommandSession(t, sessions, key+"~engineer", principal.OwnerID, "engineer")
	seedCommandSession(t, sessions, key+"~foreign", "discord:222", "main")
	now := time.Now().UTC()
	for i := range 2 {
		if _, err := events.AppendSequenced(t.Context(), key, &store.RuntimeEvent{ID: "ev-" + strconv.Itoa(i), SessionID: key, Kind: "message.completed", SchemaVersion: 1, Payload: json.RawMessage(`{}`), CreatedAt: now}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	port := &discordCommandSessions{sessions: sessions, events: events, pointers: pointers}
	resolver := &discordConversationResolver{pointers: pointers}
	if n, err := port.EventCount(t.Context(), key); err != nil || n != 2 {
		t.Errorf("EventCount = %d %v, want 2", n, err)
	}
	caller := &command.Caller{Channel: principal.ChannelDiscord, PrincipalID: principal.OwnerID, Class: principal.ClassOwner, ConversationKey: key, SessionID: key, AgentID: "main"}

	created, err := port.Create(t.Context(), caller)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !regexp.MustCompile(`^`+regexp.QuoteMeta(key)+`~[a-z2-7]{16}$`).MatchString(created.ID) || created.OwnerID != principal.OwnerID || created.AgentID != "main" {
		t.Fatalf("created = %+v", created)
	}
	if active, err := resolver.ActiveSession(t.Context(), key); err != nil || active != created.ID {
		t.Fatalf("active after /new = %q %v, want %s", active, err, created.ID)
	}
	second, err := port.Create(t.Context(), caller)
	if err != nil || second.ID == created.ID {
		t.Fatalf("second Create = %+v %v", second, err)
	}

	if sess, err := port.Switch(t.Context(), caller, created.ID); err != nil || sess.ID != created.ID {
		t.Fatalf("Switch to own pointed session = %+v %v", sess, err)
	}
	if active, _ := resolver.ActiveSession(t.Context(), key); active != created.ID {
		t.Fatalf("active after switch = %q", active)
	}
	for _, id := range []string{"main/discord:owner/dm:222", "sess_terminal", key + "~engineer", key + "~foreign", key + "~missing", key + "x"} {
		if _, err := port.Switch(t.Context(), caller, id); !isNotSwitchable(err) {
			t.Errorf("Switch(%s) error = %v, want not switchable", id, err)
		}
		if active, _ := resolver.ActiveSession(t.Context(), key); active != created.ID {
			t.Errorf("Switch(%s) moved the pointer to %s", id, active)
		}
	}
	if sess, err := port.Switch(t.Context(), caller, key); err != nil || sess.ID != key {
		t.Fatalf("Switch back to base = %+v %v", sess, err)
	}
	if _, found, _ := pointers.Resolve(t.Context(), key); found {
		t.Fatal("switching to the base key kept the pointer")
	}

	if _, err := port.Switch(t.Context(), caller, second.ID); err != nil {
		t.Fatalf("Switch(second): %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `DELETE FROM session WHERE id = ?`, second.ID); err != nil {
		t.Fatalf("gc pointed session: %v", err)
	}
	if active, err := resolver.ActiveSession(t.Context(), key); err != nil || active != key {
		t.Fatalf("active after gc = %q %v, want the base key", active, err)
	}
	if _, err := port.Switch(t.Context(), caller, second.ID); !isNotSwitchable(err) {
		t.Errorf("Switch to a collected session error = %v", err)
	}

	if _, err := port.Switch(t.Context(), caller, created.ID); err != nil {
		t.Fatalf("Switch back to the live pointed session: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `DELETE FROM session WHERE id = ?`, key); err != nil {
		t.Fatalf("drop base session row: %v", err)
	}
	sess, err := port.Switch(t.Context(), caller, key)
	if err != nil || sess.ID != key || sess.OwnerID != principal.OwnerID || sess.AgentID != "main" {
		t.Fatalf("Switch to the base key with the base row absent = %+v %v", sess, err)
	}
	if _, found, _ := pointers.Resolve(t.Context(), key); found {
		t.Fatal("switching to the base key with the base row absent kept the pointer")
	}
	ensurer := &discordSessionEnsurer{sessions: sessions}
	if err := ensurer.EnsureSession(t.Context(), key, principal.OwnerID, "main"); err != nil {
		t.Fatalf("next message re-ensures the base session: %v", err)
	}
	if _, err := sessions.Get(t.Context(), key); err != nil {
		t.Fatalf("base session row after re-ensure: %v", err)
	}
}

func isNotSwitchable(err error) bool {
	code, ok := command.CodeOf(err)
	return ok && code == command.ErrorCodeNotSwitchable
}

func TestDiscordCommandWiringDelegatesToTheEngine(t *testing.T) {
	activity := &stubActivity{}
	recorder := &wireRecorder{}
	if err := wireDiscordCommands([]runtimechannelhost.ChannelPort{}, activity, nil, nil, nil, recorder); err != nil {
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
