package command

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/principal"
)

type registryTexts struct {
	registry *agent.Registry
}

func (t registryTexts) Message(agentID, key string) string {
	if t.registry != nil && agentID != "" {
		if definition, ok := t.registry.Lookup(agentID); ok {
			return definition.Message(key)
		}
	}
	return agent.DefaultMessage(key)
}

func (t registryTexts) Persona(agentID string) string {
	if t.registry != nil && agentID != "" {
		if definition, ok := t.registry.Lookup(agentID); ok && definition.Name != "" {
			return definition.Name
		}
	}
	return agentID
}

func buildTestRegistry(t *testing.T, lang string, overrides ...agent.Persona) *agent.Registry {
	t.Helper()
	registry, err := agent.Build(overrides, nil, nil, nil, lang)
	if err != nil {
		t.Fatalf("agent.Build(%q): %v", lang, err)
	}
	return registry
}

func newLocalizedDispatcher(t *testing.T, ports *spyPorts, texts Texts) *Dispatcher {
	t.Helper()
	registry, err := New(terminalBinding(), messengerBinding())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dispatcher, err := NewDispatcher(registry, &Ports{
		Sessions: ports,
		Turns:    ports,
		Skills:   ports,
		Texts:    texts,
	}, nil)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return dispatcher
}

func TestMessengerRepliesAreLocalized(t *testing.T) {
	type replyCase struct {
		name      string
		text      string
		class     principal.Class
		sessionID string
		ports     func() *spyPorts
		wantEN    []string
		wantID    []string
	}
	cases := []replyCase{
		{
			name:      "new",
			text:      "/new",
			class:     principal.ClassOwner,
			sessionID: "sess_k",
			ports:     func() *spyPorts { return &spyPorts{} },
			wantEN:    []string{"Started a new conversation."},
			wantID:    []string{"Percakapan baru dimulai."},
		},
		{
			name:      "session current",
			text:      "/session",
			class:     principal.ClassOwner,
			sessionID: "sess_k",
			ports:     func() *spyPorts { return &spyPorts{} },
			wantEN:    []string{"This is the current conversation. Send /new to start a fresh one."},
			wantID:    []string{"Ini percakapan yang sedang aktif. Kirim /new untuk mulai yang baru."},
		},
		{
			name:      "session switched",
			text:      "/session sess_k~x",
			class:     principal.ClassOwner,
			sessionID: "sess_k",
			ports:     func() *spyPorts { return &spyPorts{} },
			wantEN:    []string{"Switched to that conversation."},
			wantID:    []string{"Sudah pindah ke percakapan itu."},
		},
		{
			name:      "status idle",
			text:      "/status",
			class:     principal.ClassOwner,
			sessionID: "sess_k",
			ports:     func() *spyPorts { return &spyPorts{events: 11} },
			wantEN:    []string{"Persona: Aura", "Nothing is running."},
			wantID:    []string{"Persona: Aura", "Tidak ada yang sedang berjalan."},
		},
		{
			name:      "status running",
			text:      "/status",
			class:     principal.ClassOwner,
			sessionID: "sess_k",
			ports:     func() *spyPorts { return &spyPorts{active: "turn-1"} },
			wantEN:    []string{"Persona: Aura", "Working on a request."},
			wantID:    []string{"Persona: Aura", "Sedang mengerjakan permintaan."},
		},
		{
			name:      "status running queued",
			text:      "/status",
			class:     principal.ClassOwner,
			sessionID: "sess_k",
			ports:     func() *spyPorts { return &spyPorts{active: "turn-1", queued: 3} },
			wantEN:    []string{"Persona: Aura", "Working on a request; 3 more waiting."},
			wantID:    []string{"Persona: Aura", "Sedang mengerjakan permintaan; 3 lagi menunggu."},
		},
		{
			name:      "cancel active",
			text:      "/cancel",
			class:     principal.ClassOwner,
			sessionID: "sess_k",
			ports:     func() *spyPorts { return &spyPorts{cancelID: "turn-1", cancelled: true} },
			wantEN:    []string{"Stopped the current request."},
			wantID:    []string{"Permintaan yang berjalan dihentikan."},
		},
		{
			name:      "cancel idle",
			text:      "/cancel",
			class:     principal.ClassOwner,
			sessionID: "sess_k",
			ports:     func() *spyPorts { return &spyPorts{} },
			wantEN:    []string{"Nothing is running."},
			wantID:    []string{"Tidak ada yang sedang berjalan."},
		},
		{
			name:      "denied",
			text:      "/status",
			class:     principal.ClassTrusted,
			sessionID: "sess_k",
			ports:     func() *spyPorts { return &spyPorts{} },
			wantEN:    []string{"That command is only available to the owner."},
			wantID:    []string{"Perintah itu hanya untuk pemilik."},
		},
		{
			name:      "guests only",
			text:      "/forgetme",
			class:     principal.ClassOwner,
			sessionID: "sess_k",
			ports:     func() *spyPorts { return &spyPorts{} },
			wantEN:    []string{"That command is not available to the owner."},
			wantID:    []string{"Perintah itu tidak tersedia untuk pemilik."},
		},
		{
			name:      "forgetme confirm",
			text:      "/forgetme",
			class:     principal.ClassTrusted,
			sessionID: "sess_k",
			ports:     func() *spyPorts { return &spyPorts{} },
			wantEN:    []string{"This erases your conversations and data with me. Send /forgetme confirm to proceed."},
			wantID:    []string{"Ini akan menghapus percakapan dan datamu bersamaku. Kirim /forgetme confirm untuk lanjut."},
		},
		{
			name:      "unavailable terminal only",
			text:      "/quit",
			class:     principal.ClassOwner,
			sessionID: "sess_k",
			ports:     func() *spyPorts { return &spyPorts{} },
			wantEN:    []string{"/quit only works in the terminal."},
			wantID:    []string{"/quit hanya bisa dipakai di terminal."},
		},
		{
			name:      "scope",
			text:      "/status",
			class:     principal.ClassOwner,
			sessionID: "",
			ports:     func() *spyPorts { return &spyPorts{} },
			wantEN:    []string{"Use /status inside an Aura thread or a DM."},
			wantID:    []string{"Pakai /status di dalam thread Aura atau DM."},
		},
		{
			name:      "session not switchable",
			text:      "/session other",
			class:     principal.ClassOwner,
			sessionID: "sess_k",
			ports:     func() *spyPorts { return &spyPorts{switchErr: Errorf(ErrorCodeNotSwitchable, "foreign")} },
			wantEN:    []string{"That conversation can't be used here."},
			wantID:    []string{"Percakapan itu tidak bisa dipakai di sini."},
		},
		{
			name:      "skill unavailable",
			text:      "/skill pdf",
			class:     principal.ClassOwner,
			sessionID: "sess_k",
			ports:     func() *spyPorts { return &spyPorts{skillErr: errors.New("missing")} },
			wantEN:    []string{"Skill pdf is not available."},
			wantID:    []string{"Skill pdf tidak tersedia."},
		},
		{
			name:      "command failed",
			text:      "/new",
			class:     principal.ClassOwner,
			sessionID: "sess_k",
			ports:     func() *spyPorts { return &spyPorts{createErr: errors.New("disk full")} },
			wantEN:    []string{"/new failed; check aura status."},
			wantID:    []string{"/new gagal; cek aura status."},
		},
		{
			name:      "usage with arguments",
			text:      "/session a b",
			class:     principal.ClassOwner,
			sessionID: "sess_k",
			ports:     func() *spyPorts { return &spyPorts{} },
			wantEN:    []string{"Usage: /session [id]"},
			wantID:    []string{"Cara pakai: /session [id]"},
		},
		{
			name:      "usage without arguments",
			text:      "/new extra",
			class:     principal.ClassOwner,
			sessionID: "sess_k",
			ports:     func() *spyPorts { return &spyPorts{} },
			wantEN:    []string{"Usage: /new"},
			wantID:    []string{"Cara pakai: /new"},
		},
		{
			name:      "unknown command",
			text:      "/bogus",
			class:     principal.ClassOwner,
			sessionID: "sess_k",
			ports:     func() *spyPorts { return &spyPorts{} },
			wantEN:    []string{"Unknown command. Try /help."},
			wantID:    []string{"Perintah tidak dikenal. Coba /help."},
		},
	}

	for _, lang := range []string{agent.DefaultMessageLanguage, agent.MessageLanguageID} {
		registry := buildTestRegistry(t, lang)
		texts := registryTexts{registry: registry}
		for _, tc := range cases {
			t.Run(lang+"/"+tc.name, func(t *testing.T) {
				d := newLocalizedDispatcher(t, tc.ports(), texts)
				c := &Caller{
					Channel:     "discord",
					PrincipalID: principal.OwnerID,
					Class:       tc.class,
					SessionID:   tc.sessionID,
					AgentID:     agent.DefaultID,
				}
				got := run(t, d, c, tc.text)
				want := tc.wantEN
				if lang == agent.MessageLanguageID {
					want = tc.wantID
				}
				if !slices.Equal(got.Lines, want) {
					t.Fatalf("lines = %q, want %q", got.Lines, want)
				}
			})
		}
	}

	t.Run("persona override wins over deployment bundle", func(t *testing.T) {
		override := agent.Persona{
			ID:           "kuma",
			Name:         "Kuma",
			Description:  "Helper",
			Instructions: "Help.",
			Messages: map[string]string{
				agent.MessageCommandNew:           "Obrolan baru siap.",
				agent.MessageCommandStatusPersona: "Asisten: {persona}",
				agent.MessageCommandUsage:         "Format /{command}: {usage}",
				agent.MessageForgetmeConfirm:      "Kirim /forgetme confirm ya.",
			},
		}
		registry := buildTestRegistry(t, agent.MessageLanguageID, override)
		d := newLocalizedDispatcher(t, &spyPorts{}, registryTexts{registry: registry})
		owner := &Caller{Channel: "discord", PrincipalID: principal.OwnerID, Class: principal.ClassOwner, SessionID: "sess_k", AgentID: "kuma"}
		guest := &Caller{Channel: "discord", PrincipalID: "discord:2", Class: principal.ClassTrusted, SessionID: "sess_k", AgentID: "kuma"}

		if got := run(t, d, owner, "/new"); !slices.Equal(got.Lines, []string{"Obrolan baru siap."}) {
			t.Errorf("/new = %q, want persona override", got.Lines)
		}
		if got := run(t, d, owner, "/status"); !slices.Equal(got.Lines, []string{"Asisten: Kuma", "Tidak ada yang sedang berjalan."}) {
			t.Errorf("/status = %q, want persona override and id fallback", got.Lines)
		}
		if got := run(t, d, owner, "/session a b"); !slices.Equal(got.Lines, []string{"Format /session: [id]"}) {
			t.Errorf("/session usage = %q, want persona override", got.Lines)
		}
		if got := run(t, d, guest, "/forgetme"); !slices.Equal(got.Lines, []string{"Kirim /forgetme confirm ya."}) {
			t.Errorf("/forgetme = %q, want persona override", got.Lines)
		}
	})

	t.Run("unknown and empty agent fall back to en and id as name", func(t *testing.T) {
		registry := buildTestRegistry(t, agent.MessageLanguageID)
		d := newLocalizedDispatcher(t, &spyPorts{active: "turn-1", queued: 2}, registryTexts{registry: registry})

		unknownCaller := &Caller{Channel: "discord", PrincipalID: principal.OwnerID, Class: principal.ClassOwner, SessionID: "sess_k", AgentID: "ghost"}
		if got := run(t, d, unknownCaller, "/new"); !slices.Equal(got.Lines, []string{"Started a new conversation."}) {
			t.Errorf("unknown /new = %q, want en default", got.Lines)
		}
		if got := run(t, d, unknownCaller, "/status"); !slices.Equal(got.Lines, []string{"Persona: ghost", "Working on a request; 2 more waiting."}) {
			t.Errorf("unknown /status = %q, want en default and agent id as name", got.Lines)
		}
		if got := run(t, d, unknownCaller, "/bogus"); !slices.Equal(got.Lines, []string{"Unknown command. Try /help."}) {
			t.Errorf("unknown /bogus = %q, want en default", got.Lines)
		}

		emptyCaller := &Caller{Channel: "discord", PrincipalID: principal.OwnerID, Class: principal.ClassOwner, SessionID: "sess_k", AgentID: ""}
		if got := run(t, d, emptyCaller, "/new"); !slices.Equal(got.Lines, []string{"Started a new conversation."}) {
			t.Errorf("empty agent /new = %q, want en default", got.Lines)
		}
		if got := run(t, d, emptyCaller, "/status"); !slices.Equal(got.Lines, []string{"Persona: ", "Working on a request; 2 more waiting."}) {
			t.Errorf("empty agent /status = %q, want en default", got.Lines)
		}
	})

	t.Run("placeholders are substituted in one pass", func(t *testing.T) {
		override := agent.Persona{
			ID:           "kuma",
			Name:         "{command}",
			Description:  "Helper",
			Instructions: "Help.",
			Messages: map[string]string{
				agent.MessageCommandUsage: "Usage: /{command} {usage}",
			},
		}
		registry := buildTestRegistry(t, agent.DefaultMessageLanguage, override)
		reg, err := NewRegistry(
			append(Catalog(), Definition{Name: "custom", Usage: "{command}", Summary: "custom", Audience: AudienceOwner, Kind: KindLocal, MinArgs: 1, MaxArgs: 1}),
			Binding{Channel: "discord", Style: StyleMessenger, Names: append(MessengerNames(), "custom")},
		)
		if err != nil {
			t.Fatalf("NewRegistry: %v", err)
		}
		d, err := NewDispatcher(reg, &Ports{Sessions: &spyPorts{}, Texts: registryTexts{registry: registry}}, nil)
		if err != nil {
			t.Fatalf("NewDispatcher: %v", err)
		}
		owner := &Caller{Channel: "discord", PrincipalID: principal.OwnerID, Class: principal.ClassOwner, SessionID: "sess_k", AgentID: "kuma"}
		if got := run(t, d, owner, "/custom"); !slices.Equal(got.Lines, []string{"Usage: /custom {command}"}) {
			t.Fatalf("one-pass usage = %q, want literal {command} in usage preserved", got.Lines)
		}
	})
}

type fixtureIDPorts struct {
	spyPorts
	createdID string
}

func (f *fixtureIDPorts) Create(_ context.Context, caller *Caller) (Session, error) {
	return Session{ID: f.createdID, OwnerID: caller.PrincipalID, AgentID: caller.AgentID}, nil
}

func TestMessengerRepliesCarryNoIds(t *testing.T) {
	const (
		fixtureSessionID = "main/telegram:owner/chat:1065778107~yvbrebuzzfotkhmm"
		createdSessionID = "main/telegram:owner/chat:1065778107~fresh9876543210a"
		switchTargetID   = "main/telegram:owner/chat:1065778107~other1234567890b"
		fixtureTurnID    = "turn-01j9x7k3m2n4p5q6r7s8t9u0vw"
		fixtureEvents    = 4719
	)

	forbidden := []string{fixtureSessionID, createdSessionID, switchTargetID, fixtureTurnID, "yvbrebuzzfotkhmm", "fresh9876543210a", "other1234567890b", "4719"}
	commands := []string{"/new", "/session", "/session " + switchTargetID, "/status", "/cancel"}

	for _, lang := range []string{agent.DefaultMessageLanguage, agent.MessageLanguageID} {
		registry := buildTestRegistry(t, lang)
		texts := registryTexts{registry: registry}
		for _, state := range []struct {
			name      string
			active    string
			queued    int
			cancelled bool
		}{
			{name: "idle"},
			{name: "running", active: fixtureTurnID, cancelled: true},
			{name: "queued", active: fixtureTurnID, queued: 2, cancelled: true},
		} {
			t.Run(lang+"/"+state.name, func(t *testing.T) {
				ports := &fixtureIDPorts{
					spyPorts: spyPorts{
						events:    fixtureEvents,
						active:    state.active,
						queued:    state.queued,
						cancelID:  state.active,
						cancelled: state.cancelled,
					},
					createdID: createdSessionID,
				}
				reg, err := New(terminalBinding(), messengerBinding())
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				d, err := NewDispatcher(reg, &Ports{Sessions: ports, Turns: ports, Skills: ports, Texts: texts}, nil)
				if err != nil {
					t.Fatalf("NewDispatcher: %v", err)
				}
				owner := &Caller{
					Channel:         "discord",
					PrincipalID:     principal.OwnerID,
					Class:           principal.ClassOwner,
					ConversationKey: "main/telegram:owner/chat:1065778107",
					SessionID:       fixtureSessionID,
					AgentID:         agent.DefaultID,
				}
				for _, cmd := range commands {
					got := run(t, d, owner, cmd)
					joined := strings.Join(got.Lines, "\n")
					if joined == "" {
						t.Fatalf("%s returned empty reply", cmd)
					}
					for _, leak := range forbidden {
						if strings.Contains(joined, leak) {
							t.Errorf("%s reply %q leaked %q", cmd, joined, leak)
						}
					}
				}
			})
		}
	}
}

func TestTerminalRepliesAreUnchanged(t *testing.T) {
	registry := buildTestRegistry(t, agent.MessageLanguageID)
	texts := registryTexts{registry: registry}

	ports := &spyPorts{events: 7, active: "turn_1", queued: 2, cancelID: "turn_1", cancelled: true}
	d := newLocalizedDispatcher(t, ports, texts)
	owner := caller(ChannelTerminal, principal.ClassOwner)

	if got := run(t, d, owner, "/status"); !slices.Equal(got.Lines, []string{"session sess_k", "persona aura", "events 7", "turn running turn_1 queued 2"}) {
		t.Errorf("terminal /status = %q", got.Lines)
	}
	if got := run(t, d, owner, "/session"); !slices.Equal(got.Lines, []string{"session sess_k"}) || got.Diagnostic {
		t.Errorf("terminal /session = %+v", got)
	}
	if got := run(t, d, owner, "/session sess_k~x"); !slices.Equal(got.Lines, []string{"switched to session sess_k~x"}) {
		t.Errorf("terminal /session id = %q", got.Lines)
	}
	if got := run(t, d, owner, "/new"); !slices.Equal(got.Lines, []string{"new session sess_new"}) {
		t.Errorf("terminal /new = %q", got.Lines)
	}
	if got := run(t, d, owner, "/cancel"); !slices.Equal(got.Lines, []string{"cancelled turn turn_1"}) {
		t.Errorf("terminal /cancel = %q", got.Lines)
	}
	if got := run(t, d, owner, "/session a b"); got.Code != ErrorCodeUsage || !slices.Equal(got.Lines, []string{"usage: /session [id]"}) {
		t.Errorf("terminal /session usage = %+v, want lowercase usage:", got)
	}
	if got := run(t, d, owner, "/new extra"); got.Code != ErrorCodeUsage || !slices.Equal(got.Lines, []string{"usage: /new"}) {
		t.Errorf("terminal /new usage = %+v, want lowercase usage:", got)
	}
	if got := run(t, d, owner, "/Bogus x"); got.Code != ErrorCodeUnknown || !slices.Equal(got.Lines, []string{"aura: unknown command /Bogus (try /help)"}) {
		t.Errorf("terminal /Bogus = %+v", got)
	}
	if got := run(t, d, owner, "/skill-create n"); got.Pending == nil || !slices.Equal(got.Lines, []string{"describe the skill in one line:"}) {
		t.Errorf("terminal /skill-create prompt = %+v", got)
	}

	idle := newLocalizedDispatcher(t, &spyPorts{events: 3}, texts)
	if got := run(t, idle, owner, "/status"); !slices.Equal(got.Lines, []string{"session sess_k", "persona aura", "events 3", "turn idle"}) {
		t.Errorf("terminal idle /status = %q", got.Lines)
	}
	if got := run(t, idle, owner, "/cancel"); !slices.Equal(got.Lines, []string{"no active turn"}) {
		t.Errorf("terminal idle /cancel = %q", got.Lines)
	}

	failing := newLocalizedDispatcher(t, &spyPorts{
		switchErr: Errorf(ErrorCodeNotSwitchable, "not owned by local-owner"),
		createErr: errors.New("disk full"),
		skillErr:  errors.New("skill is not available"),
	}, texts)
	if got := run(t, failing, owner, "/session other"); !slices.Equal(got.Lines, []string{"aura: session other: not owned by local-owner"}) {
		t.Errorf("terminal /session err = %q", got.Lines)
	}
	if got := run(t, failing, owner, "/new"); !slices.Equal(got.Lines, []string{"aura: /new: disk full"}) {
		t.Errorf("terminal /new err = %q", got.Lines)
	}
	if got := run(t, failing, owner, "/skill pdf"); !slices.Equal(got.Lines, []string{"aura: skill pdf: skill is not available"}) {
		t.Errorf("terminal /skill err = %q", got.Lines)
	}
}
