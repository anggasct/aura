package command

import (
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/principal"
)

func messengerBinding() Binding {
	return Binding{
		Channel: "discord",
		Style:   StyleMessenger,
		Names:   []string{"help", "status", "session", "new", "cancel", "skill", "skill-create"},
	}
}

func terminalBinding() Binding {
	return Binding{
		Channel:     ChannelTerminal,
		Style:       StyleTerminal,
		Interactive: true,
		Names:       []string{"help", "status", "session", "new", "cancel", "skill", "skill-create", "exit", "clear"},
		Notes:       []Note{{Syntax: ".", Summary: "compose a multi-line prompt in $EDITOR (interactive)"}},
	}
}

func TestCatalogBuildsWithBothBindings(t *testing.T) {
	if _, err := New(terminalBinding(), messengerBinding()); err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestRegistryRejectsInvalidDefinitionsAndBindings(t *testing.T) {
	valid := Definition{Name: "ping", Summary: "ping", Audience: AudienceAny, Kind: KindLocal}
	with := func(edit func(*Definition)) Definition {
		def := valid
		edit(&def)
		return def
	}
	tests := []struct {
		name     string
		defs     []Definition
		bindings []Binding
		want     string
	}{
		{"duplicate name", []Definition{valid, valid}, nil, "defined more than once"},
		{"alias collides with name", []Definition{valid, with(func(d *Definition) { d.Name, d.Aliases = "pong", []string{"ping"} })}, nil, "defined more than once"},
		{"alias collides with alias", []Definition{
			with(func(d *Definition) { d.Aliases = []string{"p"} }),
			with(func(d *Definition) { d.Name, d.Aliases = "pong", []string{"p"} }),
		}, nil, "defined more than once"},
		{"invalid name", []Definition{with(func(d *Definition) { d.Name = "Ping" })}, nil, "is not valid"},
		{"invalid alias", []Definition{with(func(d *Definition) { d.Aliases = []string{"p_q"} })}, nil, "is not valid"},
		{"name too long", []Definition{with(func(d *Definition) { d.Name = "a" + strings.Repeat("b", 32) })}, nil, "is not valid"},
		{"unknown audience", []Definition{with(func(d *Definition) { d.Audience = "guest" })}, nil, "unknown audience"},
		{"unknown kind", []Definition{with(func(d *Definition) { d.Kind = "remote" })}, nil, "unknown kind"},
		{"terminal-only without control", []Definition{with(func(d *Definition) { d.Kind = KindTerminalOnly })}, nil, "needs a control"},
		{"local with control", []Definition{with(func(d *Definition) { d.Control = ControlExit })}, nil, "must not carry a control"},
		{"negative arity", []Definition{with(func(d *Definition) { d.MinArgs = -1 })}, nil, "invalid arity"},
		{"max below min", []Definition{with(func(d *Definition) { d.MinArgs, d.MaxArgs = 2, 1 })}, nil, "invalid arity"},
		{"prompt without required argument", []Definition{with(func(d *Definition) { d.Prompt = "?" })}, nil, "prompts but"},
		{"missing summary", []Definition{with(func(d *Definition) { d.Summary = " " })}, nil, "needs a summary"},
		{"binding unknown name", []Definition{valid}, []Binding{{Channel: "x", Style: StyleMessenger, Names: []string{"pong"}}}, "binds unknown command"},
		{"binding by alias", []Definition{with(func(d *Definition) { d.Aliases = []string{"p"} })}, []Binding{{Channel: "x", Style: StyleMessenger, Names: []string{"p"}}}, "binds unknown command"},
		{"binding twice", []Definition{valid}, []Binding{{Channel: "x", Style: StyleMessenger, Names: []string{"ping", "ping"}}}, "more than once"},
		{"channel bound twice", []Definition{valid}, []Binding{{Channel: "x", Style: StyleMessenger}, {Channel: "x", Style: StyleMessenger}}, "bound more than once"},
		{"empty channel", []Definition{valid}, []Binding{{Style: StyleMessenger}}, "channel must not be empty"},
		{"unknown style", []Definition{valid}, []Binding{{Channel: "x", Style: "web"}}, "unknown style"},
		{"terminal-only in messenger", []Definition{with(func(d *Definition) { d.Kind, d.Control = KindTerminalOnly, ControlExit })}, []Binding{{Channel: "x", Style: StyleMessenger, Names: []string{"ping"}}}, "cannot bind terminal-only"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewRegistry(tt.defs, tt.bindings...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("NewRegistry() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestEachCatalogCommandIsDefinedOnce(t *testing.T) {
	seen := map[string]bool{}
	for _, def := range Catalog() {
		for _, name := range append([]string{def.Name}, def.Aliases...) {
			if seen[name] {
				t.Errorf("catalog defines %q twice", name)
			}
			seen[name] = true
		}
	}
	for _, name := range []string{"help", "status", "session", "new", "cancel", "skill", "skill-create", "exit", "quit", "clear"} {
		if !seen[name] {
			t.Errorf("catalog is missing %q", name)
		}
	}
}

func TestLookupResolvesAliasesAndBoundness(t *testing.T) {
	registry, err := New(terminalBinding(), messengerBinding())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	def, ok := registry.Lookup("quit")
	if !ok || def.Name != "exit" {
		t.Fatalf("Lookup(quit) = %+v %v, want exit", def, ok)
	}
	if !registry.Bound(ChannelTerminal, "quit") {
		t.Error("terminal should bind /quit through /exit")
	}
	if registry.Bound("discord", "exit") || registry.Bound("discord", "bogus") || registry.Bound("web", "help") {
		t.Error("messenger must not bind terminal-only, unknown, or unbound channels")
	}
}

func TestHelpGoldenPerChannelAndClass(t *testing.T) {
	registry, err := New(terminalBinding(), messengerBinding())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tests := []struct {
		name    string
		channel string
		class   principal.Class
		want    string
	}{
		{"owner terminal", ChannelTerminal, principal.ClassOwner, `commands:
  /help                                show this help
  /status                              show the current session
  /session [id]                        show or switch to a session
  /new                                 start a new session
  /cancel                              cancel the active turn
  /skill <name> [request…]             activate a skill
  /skill-create <name> <description…>  stage a skill draft
  /exit, /quit                         leave the console
  /clear                               clear the screen
  .                                    compose a multi-line prompt in $EDITOR (interactive)`},
		{"owner messenger", "discord", principal.ClassOwner, `commands:
  /help                                show this help
  /status                              show the current session
  /session [id]                        show or switch to a session
  /new                                 start a new session
  /cancel                              cancel the active turn
  /skill <name> [request…]             activate a skill
  /skill-create <name> <description…>  stage a skill draft`},
		{"trusted messenger", "discord", principal.ClassTrusted, `commands:
  /help  show this help`},
	}
	outputs := map[string]bool{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strings.Join(registry.Help(tt.channel, tt.class), "\n")
			if got != tt.want {
				t.Errorf("Help() =\n%s\nwant\n%s", got, tt.want)
			}
			outputs[got] = true
		})
	}
	if len(outputs) != len(tests) {
		t.Errorf("help outputs are not distinct: %d of %d", len(outputs), len(tests))
	}
}
