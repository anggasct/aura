package command

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/principal"
)

type spyPorts struct {
	calls      []string
	events     int
	eventsErr  error
	active     string
	queued     int
	cancelID   string
	cancelled  bool
	switchErr  error
	createErr  error
	skillErr   error
	draftErr   error
	switchedTo string
	draftDesc  string
}

func (s *spyPorts) EventCount(context.Context, string) (int, error) {
	s.calls = append(s.calls, "EventCount")
	return s.events, s.eventsErr
}

func (s *spyPorts) Create(_ context.Context, caller *Caller) (Session, error) {
	s.calls = append(s.calls, "Create")
	if s.createErr != nil {
		return Session{}, s.createErr
	}
	return Session{ID: "sess_new", OwnerID: caller.PrincipalID, AgentID: caller.AgentID}, nil
}

func (s *spyPorts) Switch(_ context.Context, caller *Caller, id string) (Session, error) {
	s.calls = append(s.calls, "Switch")
	if s.switchErr != nil {
		return Session{}, s.switchErr
	}
	s.switchedTo = id
	return Session{ID: id, OwnerID: caller.PrincipalID, AgentID: "engineer"}, nil
}

func (s *spyPorts) CancelActive(context.Context, string) (turnID string, cancelled bool, err error) {
	s.calls = append(s.calls, "CancelActive")
	return s.cancelID, s.cancelled, nil
}

func (s *spyPorts) Activity(string) (activeTurnID string, queued int) {
	s.calls = append(s.calls, "Activity")
	return s.active, s.queued
}

func (s *spyPorts) Activate(_ context.Context, name string) (Activation, error) {
	s.calls = append(s.calls, "Activate")
	if s.skillErr != nil {
		return Activation{}, s.skillErr
	}
	return Activation{SkillID: "local/" + name + "@0123456789ab", Caveat: "untrusted", Text: "Do things."}, nil
}

func (s *spyPorts) StageDraft(_ context.Context, name, description string) (Draft, error) {
	s.calls = append(s.calls, "StageDraft")
	s.draftDesc = description
	if s.draftErr != nil {
		return Draft{}, s.draftErr
	}
	return Draft{ID: "local/" + name + "@0123456789ab", Digest: "0123456789abcdef"}, nil
}

type observed struct {
	name, channel, class, result string
}

type spyRecorder struct {
	seen []observed
}

func (r *spyRecorder) Start(ctx context.Context, name, channel, class string) (spanCtx context.Context, finish func(string)) {
	return ctx, func(result string) {
		r.seen = append(r.seen, observed{name: name, channel: channel, class: class, result: result})
	}
}

func newTestDispatcher(t *testing.T, ports *spyPorts, recorder Recorder) *Dispatcher {
	t.Helper()
	registry, err := New(terminalBinding(), messengerBinding())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dispatcher, err := NewDispatcher(registry, Ports{Sessions: ports, Turns: ports, Skills: ports}, recorder)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return dispatcher
}

func caller(channel string, class principal.Class) *Caller {
	return &Caller{Channel: channel, PrincipalID: "local-owner", Class: class, SessionID: "sess_k", AgentID: "aura"}
}

func run(t *testing.T, d *Dispatcher, c *Caller, text string) Result {
	t.Helper()
	inv, ok := Parse(text)
	if !ok {
		t.Fatalf("Parse(%q) failed", text)
	}
	return d.Execute(t.Context(), c, &inv)
}

func TestNewDispatcherRequiresRegistryAndSessions(t *testing.T) {
	registry, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := NewDispatcher(nil, Ports{Sessions: &spyPorts{}}, nil); err == nil {
		t.Error("nil registry accepted")
	}
	if _, err := NewDispatcher(registry, Ports{}, nil); err == nil {
		t.Error("nil sessions accepted")
	}
}

func TestAuthorizationMatrix(t *testing.T) {
	invocations := map[string]string{
		"help":         "/help",
		"status":       "/status",
		"session":      "/session sess_k~x",
		"new":          "/new",
		"cancel":       "/cancel",
		"skill":        "/skill pdf now",
		"skill-create": "/skill-create n A skill.",
	}
	for _, class := range []principal.Class{principal.ClassOwner, principal.ClassTrusted, principal.ClassPublic} {
		for _, def := range Catalog() {
			if def.Kind == KindTerminalOnly {
				continue
			}
			t.Run(string(class)+"/"+def.Name, func(t *testing.T) {
				ports := &spyPorts{}
				recorder := &spyRecorder{}
				result := run(t, newTestDispatcher(t, ports, recorder), caller("discord", class), invocations[def.Name])
				allowed := def.Audience == AudienceAny || class == principal.ClassOwner
				if allowed && result.Code == CodeDenied {
					t.Fatalf("%s denied for %s", def.Name, class)
				}
				if !allowed {
					if result.Code != CodeDenied || !slices.Equal(result.Lines, []string{ReplyDenied}) {
						t.Fatalf("result = %+v, want denial", result)
					}
					if len(ports.calls) != 0 {
						t.Errorf("denied command reached ports: %v", ports.calls)
					}
				}
				want := ResultOK
				if !allowed {
					want = ResultDenied
				}
				if len(recorder.seen) != 1 || recorder.seen[0] != (observed{def.Name, "discord", string(class), want}) {
					t.Errorf("observed = %+v, want %s %s", recorder.seen, def.Name, want)
				}
			})
		}
	}
}

func TestTerminalOnlyNamesAreUnavailableInMessengers(t *testing.T) {
	for _, text := range []string{"/exit", "/quit", "/clear", "/QUIT now"} {
		t.Run(text, func(t *testing.T) {
			recorder := &spyRecorder{}
			result := run(t, newTestDispatcher(t, &spyPorts{}, recorder), caller("discord", principal.ClassTrusted), text)
			name := strings.ToLower(strings.Fields(text)[0])
			if result.Code != CodeUnavailable || result.Lines[0] != name+" only works in the terminal." || result.Control != ControlNone {
				t.Fatalf("result = %+v", result)
			}
			if len(recorder.seen) != 1 || recorder.seen[0].name != NameTerminalOnly || recorder.seen[0].result != ResultUnavailable {
				t.Errorf("observed = %+v", recorder.seen)
			}
		})
	}
}

func TestTerminalOnlyNamesControlTheTerminal(t *testing.T) {
	d := newTestDispatcher(t, &spyPorts{}, nil)
	if got := run(t, d, caller(ChannelTerminal, principal.ClassOwner), "/quit"); got.Control != ControlExit {
		t.Errorf("/quit = %+v", got)
	}
	if got := run(t, d, caller(ChannelTerminal, principal.ClassOwner), "/clear"); got.Control != ControlClear {
		t.Errorf("/clear = %+v", got)
	}
	if got := run(t, d, caller(ChannelTerminal, principal.ClassOwner), "/exit now"); got.Code != CodeUsage || got.Control != ControlNone {
		t.Errorf("/exit now = %+v, want usage", got)
	}
}

func TestUnknownAndUnboundNames(t *testing.T) {
	recorder := &spyRecorder{}
	d := newTestDispatcher(t, &spyPorts{}, recorder)
	got := run(t, d, caller(ChannelTerminal, principal.ClassOwner), "/Bogus x")
	if got.Code != CodeUnknown || got.Lines[0] != "aura: unknown command /Bogus (try /help)" || !got.Diagnostic {
		t.Errorf("unknown = %+v", got)
	}
	if got := run(t, d, caller("web", principal.ClassOwner), "/help"); got.Code != CodeUnknown {
		t.Errorf("unbound channel = %+v", got)
	}
	if len(recorder.seen) != 0 {
		t.Errorf("unknown names were counted: %+v", recorder.seen)
	}
}

func TestStrictArity(t *testing.T) {
	tests := []struct {
		text string
		want string
	}{
		{"/help me", "usage: /help"},
		{"/status now", "usage: /status"},
		{"/new session", "usage: /new"},
		{"/cancel all", "usage: /cancel"},
		{"/session a b", "usage: /session [id]"},
		{"/skill", "usage: /skill <name> [request…]"},
		{"/skill-create", "usage: /skill-create <name> <description…>"},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			ports := &spyPorts{}
			got := run(t, newTestDispatcher(t, ports, nil), caller("discord", principal.ClassOwner), tt.text)
			if got.Code != CodeUsage || !slices.Equal(got.Lines, []string{tt.want}) || !got.Diagnostic {
				t.Errorf("result = %+v, want %q", got, tt.want)
			}
			if len(ports.calls) != 0 {
				t.Errorf("usage error reached ports: %v", ports.calls)
			}
		})
	}
}

func TestSkillCreatePromptsOnlyInInteractiveBindings(t *testing.T) {
	ports := &spyPorts{}
	d := newTestDispatcher(t, ports, nil)
	got := run(t, d, caller(ChannelTerminal, principal.ClassOwner), "/skill-create n")
	if got.Pending == nil || got.Pending.Name != "skill-create" || !slices.Equal(got.Pending.Args, []string{"n"}) || got.Lines[0] != "describe the skill in one line:" {
		t.Fatalf("terminal = %+v", got)
	}
	resumed := *got.Pending
	resumed.Args = append(resumed.Args, "A prompted  description.")
	done := d.Execute(t.Context(), caller(ChannelTerminal, principal.ClassOwner), &resumed)
	if done.Code != CodeOK || ports.draftDesc != "A prompted  description." {
		t.Fatalf("resumed = %+v desc %q", done, ports.draftDesc)
	}
	want := []string{"draft local/n@0123456789ab", "digest 0123456789abcdef", "review with: aura skills review local/n@0123456789ab"}
	if !slices.Equal(done.Lines, want) {
		t.Errorf("lines = %q", done.Lines)
	}
	if got := run(t, d, caller("discord", principal.ClassOwner), "/skill-create n"); got.Code != CodeUsage || got.Pending != nil {
		t.Errorf("messenger = %+v, want usage", got)
	}
}

func TestSessionScopedCommandsNeedAConversationSession(t *testing.T) {
	d := newTestDispatcher(t, &spyPorts{}, nil)
	parent := caller("discord", principal.ClassOwner)
	parent.SessionID = ""
	for _, name := range []string{"status", "session", "new", "cancel"} {
		got := run(t, d, parent, "/"+name)
		if got.Code != CodeScope || got.Lines[0] != "Use /"+name+" inside an Aura thread or a DM." {
			t.Errorf("/%s = %+v", name, got)
		}
	}
	if got := run(t, d, parent, "/help"); got.Code != CodeOK {
		t.Errorf("/help = %+v", got)
	}
}

func TestCanonicalOutcomes(t *testing.T) {
	ports := &spyPorts{events: 7, active: "turn_1", queued: 2, cancelID: "turn_1", cancelled: true}
	d := newTestDispatcher(t, ports, nil)
	owner := caller("discord", principal.ClassOwner)
	if got := run(t, d, owner, "/status"); !slices.Equal(got.Lines, []string{"session sess_k", "persona aura", "events 7", "turn running turn_1 queued 2"}) {
		t.Errorf("/status = %q", got.Lines)
	}
	if got := run(t, d, owner, "/session"); !slices.Equal(got.Lines, []string{"session sess_k"}) || got.Diagnostic {
		t.Errorf("/session = %+v", got)
	}
	got := run(t, d, owner, "/session sess_k~x")
	if !slices.Equal(got.Lines, []string{"switched to session sess_k~x"}) || got.Session == nil || got.Session.AgentID != "engineer" {
		t.Errorf("/session id = %+v", got)
	}
	got = run(t, d, owner, "/new")
	if !slices.Equal(got.Lines, []string{"new session sess_new"}) || got.Session == nil || got.Session.ID != "sess_new" {
		t.Errorf("/new = %+v", got)
	}
	if got := run(t, d, owner, "/cancel"); !slices.Equal(got.Lines, []string{"cancelled turn turn_1"}) {
		t.Errorf("/cancel = %q", got.Lines)
	}
	idle := &spyPorts{}
	d = newTestDispatcher(t, idle, nil)
	if got := run(t, d, owner, "/status"); got.Lines[3] != "turn idle" {
		t.Errorf("idle /status = %q", got.Lines)
	}
	if got := run(t, d, owner, "/cancel"); !slices.Equal(got.Lines, []string{"no active turn"}) {
		t.Errorf("idle /cancel = %q", got.Lines)
	}
	if got := run(t, d, owner, "/skill pdf"); !slices.Equal(got.Lines, []string{"skill local/pdf@0123456789ab", "untrusted", "Do things."}) {
		t.Errorf("/skill = %q", got.Lines)
	}
}

func TestErrorRenderingPerStyle(t *testing.T) {
	ports := &spyPorts{
		switchErr: Errorf(CodeNotSwitchable, "not owned by local-owner"),
		createErr: errors.New("create: disk full at /var/lib/aura"),
		skillErr:  errors.New("skill_unavailable: skill is not available"),
		draftErr:  errors.New("invalid name"),
		eventsErr: errors.New("database is locked"),
	}
	d := newTestDispatcher(t, ports, nil)
	tests := []struct {
		text     string
		code     Code
		terminal string
		discord  string
	}{
		{"/session other", CodeNotSwitchable, "aura: session other: not owned by local-owner", ReplyNotSwitchable},
		{"/new", CodeFailed, "aura: /new: create: disk full at /var/lib/aura", "/new failed; check aura status."},
		{"/status", CodeFailed, "aura: /status: database is locked", "/status failed; check aura status."},
		{"/skill pdf", CodeSkillUnavailable, "aura: skill pdf: skill_unavailable: skill is not available", "Skill pdf is not available."},
		{"/skill-create n A b", CodeFailed, "aura: /skill-create: invalid name", "/skill-create failed; check aura status."},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			term := run(t, d, caller(ChannelTerminal, principal.ClassOwner), tt.text)
			disc := run(t, d, caller("discord", principal.ClassOwner), tt.text)
			if term.Code != tt.code || disc.Code != tt.code || !term.Diagnostic || !disc.Diagnostic {
				t.Fatalf("codes = %s %s, want %s", term.Code, disc.Code, tt.code)
			}
			if term.Lines[0] != tt.terminal || disc.Lines[0] != tt.discord {
				t.Errorf("lines = %q / %q", term.Lines[0], disc.Lines[0])
			}
		})
	}
}

func TestMissingOptionalPorts(t *testing.T) {
	registry, err := New(messengerBinding())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d, err := NewDispatcher(registry, Ports{Sessions: &spyPorts{}}, nil)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	owner := caller("discord", principal.ClassOwner)
	if got := run(t, d, owner, "/cancel"); got.Lines[0] != "no active turn" {
		t.Errorf("/cancel = %+v", got)
	}
	if got := run(t, d, owner, "/skill pdf"); got.Code != CodeSkillUnavailable {
		t.Errorf("/skill = %+v", got)
	}
	if got := run(t, d, owner, "/skill-create n A b"); got.Code != CodeFailed {
		t.Errorf("/skill-create = %+v", got)
	}
}
