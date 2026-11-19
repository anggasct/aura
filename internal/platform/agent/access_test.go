package agent

import (
	"slices"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
)

func kantorOverride() Persona {
	return Persona{
		ID:              "kantor",
		Name:            "Kantor",
		Description:     "Office assistant",
		Instructions:    "Help the team.",
		Audience:        AudienceTrusted,
		Tools:           []string{"web_search", "web_fetch", "read_file", "write_file"},
		PrincipalTools:  []string{"web_search", "write_file"},
		PublicTools:     []string{"web_search"},
		ApprovalTimeout: configkit.Duration(10 * time.Minute),
		Messages:        map[string]string{MessageWaitingForOwner: "Sebentar ya."},
	}
}

func TestBuildCarriesAccessFields(t *testing.T) {
	registry, err := Build([]Persona{kantorOverride()}, validTools, []string{"primary"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	kantor, _ := registry.Lookup("kantor")
	if kantor.Audience != AudienceTrusted || kantor.ApprovalTimeout != 10*time.Minute {
		t.Fatalf("kantor access = %q %v", kantor.Audience, kantor.ApprovalTimeout)
	}
	if got := kantor.Message(MessageWaitingForOwner); got != "Sebentar ya." {
		t.Errorf("override message = %q", got)
	}
	if got, want := kantor.Message(MessageApprovalTimeout), PersonaMessageDefaults()[MessageApprovalTimeout]; got != want {
		t.Errorf("default message = %q, want %q", got, want)
	}
	for _, id := range []string{"main", "engineer", "reviewer", "researcher"} {
		builtin, _ := registry.Lookup(id)
		if builtin.Audience != AudienceOwner {
			t.Errorf("%s audience = %q, want owner by default", id, builtin.Audience)
		}
	}
	custom := kantorOverride()
	custom.Audience = ""
	registry, err = Build([]Persona{custom}, validTools, []string{"primary"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if defaulted, _ := registry.Lookup("kantor"); defaulted.Audience != AudienceOwner {
		t.Errorf("unset audience = %q, want owner", defaulted.Audience)
	}
}

func TestDefinitionAdmitsByAudience(t *testing.T) {
	classes := []principal.Class{principal.ClassOwner, principal.ClassTrusted, principal.ClassPublic, ""}
	want := map[string][]bool{
		AudienceOwner:   {true, false, false, false},
		AudienceTrusted: {true, true, false, true},
		AudiencePublic:  {true, true, true, true},
	}
	for audience, admits := range want {
		definition := Definition{Audience: audience}
		for i, class := range classes {
			if got := definition.Admits(class); got != admits[i] {
				t.Errorf("audience %s admits %q = %v, want %v", audience, class, got, admits[i])
			}
		}
	}
}

func TestDefinitionToolCeilingPerClass(t *testing.T) {
	registry, err := Build([]Persona{kantorOverride()}, validTools, []string{"primary"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	kantor, _ := registry.Lookup("kantor")
	cases := map[principal.Class]struct {
		tools      []string
		restricted bool
	}{
		principal.ClassOwner:   {[]string{"web_search", "web_fetch", "read_file", "write_file"}, false},
		principal.ClassTrusted: {[]string{"web_search", "write_file"}, true},
		principal.ClassPublic:  {[]string{"web_search"}, true},
	}
	for class, want := range cases {
		tools, restricted := kantor.ToolCeiling(class)
		if !slices.Equal(tools, want.tools) || restricted != want.restricted {
			t.Errorf("ToolCeiling(%s) = %v %v, want %v %v", class, tools, restricted, want.tools, want.restricted)
		}
	}
}

func TestBuildRejectsInvalidToolCeilings(t *testing.T) {
	cases := map[string]func(*Persona){
		"public effect tool":           func(d *Persona) { d.PublicTools = []string{"write_file"} },
		"principal tool outside tools": func(d *Persona) { d.PrincipalTools = []string{"exec"} },
		"public tool outside tools":    func(d *Persona) { d.PublicTools = []string{"list_dir"} },
		"public exec without tools": func(d *Persona) {
			d.Tools = nil
			d.PublicTools = []string{"exec"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			override := kantorOverride()
			mutate(&override)
			_, err := Build([]Persona{override}, validTools, []string{"primary"})
			if !fault.HasCode(err, ErrorCodePersonaInvalid) {
				t.Fatalf("Build() error = %v, want code %s", err, ErrorCodePersonaInvalid)
			}
		})
	}
	unrestricted := kantorOverride()
	unrestricted.Tools = nil
	unrestricted.PrincipalTools = []string{"exec"}
	unrestricted.PublicTools = []string{"list_dir"}
	if _, err := Build([]Persona{unrestricted}, validTools, []string{"primary"}); err != nil {
		t.Fatalf("persona without a tools list accepts any read-tier ceiling: %v", err)
	}
}

func TestRouterServesPersonaByAccountDefaultOrBinding(t *testing.T) {
	public := kantorOverride()
	public.ID = "lobby"
	public.Audience = AudiencePublic
	registry, err := Build([]Persona{kantorOverride(), public}, validTools, []string{"primary"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	accounts := []ChannelAccount{
		{Channel: BindingChannelDiscord, Name: "home", Persona: DefaultID, Field: "channels.discord.accounts[0]"},
		{Channel: BindingChannelDiscord, Name: "office", Persona: "kantor", Field: "channels.discord.accounts[1]"},
		{Channel: BindingChannelTelegram, Name: "pocket", Persona: DefaultID, Field: "channels.telegram.accounts[0]"},
	}
	router, err := NewRouter(registry, accounts, []Binding{discordBinding("lobby", "home", Binding{ChannelID: "9"})})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	cases := []struct {
		name    string
		channel string
		account string
		persona string
		want    bool
	}{
		{"account default", BindingChannelDiscord, "office", "kantor", true},
		{"binding on the account", BindingChannelDiscord, "home", "lobby", true},
		{"default of the same account", BindingChannelDiscord, "home", DefaultID, true},
		{"persona bound to another account", BindingChannelDiscord, "office", "lobby", false},
		{"same account name on another channel", BindingChannelTelegram, "home", DefaultID, false},
		{"telegram account default", BindingChannelTelegram, "pocket", DefaultID, true},
		{"unknown account", BindingChannelDiscord, "missing", "kantor", false},
		{"blank persona", BindingChannelDiscord, "home", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := router.ServesPersona(tc.channel, tc.account, tc.persona); got != tc.want {
				t.Errorf("ServesPersona(%s, %s, %q) = %v, want %v", tc.channel, tc.account, tc.persona, got, tc.want)
			}
		})
	}
	var missing *Router
	if missing.ServesPersona(BindingChannelDiscord, "home", DefaultID) {
		t.Error("a nil router serves a persona")
	}
}

func TestRouterServesPublicOnlyWhenAPublicPersonaIsReachable(t *testing.T) {
	public := kantorOverride()
	public.ID = "lobby"
	public.Audience = AudiencePublic
	registry, err := Build([]Persona{kantorOverride(), public}, validTools, []string{"primary"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	accounts := []ChannelAccount{
		{Channel: BindingChannelDiscord, Name: "home", Persona: DefaultID, Field: "channels.discord.accounts[0]"},
		{Channel: BindingChannelDiscord, Name: "office", Persona: "kantor", Field: "channels.discord.accounts[1]"},
		{Channel: BindingChannelDiscord, Name: "front", Persona: "lobby", Field: "channels.discord.accounts[2]"},
	}
	router, err := NewRouter(registry, accounts, []Binding{discordBinding("lobby", "home", Binding{ChannelID: "9"})})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	for account, want := range map[string]bool{"home": true, "office": false, "front": true, "missing": false} {
		if got := router.ServesPublic(BindingChannelDiscord, account); got != want {
			t.Errorf("ServesPublic(%s) = %v, want %v", account, got, want)
		}
	}
	if definition, ok := router.Persona("kantor"); !ok || definition.Name != "Kantor" {
		t.Errorf("Persona(kantor) = %+v, %v", definition, ok)
	}
}

func TestGroupContextDefaultsToPerPrincipal(t *testing.T) {
	registry, err := Build([]Persona{
		{ID: "crew", Description: "d", Instructions: "i", Audience: AudienceTrusted, GroupContext: GroupContextShared},
		{ID: "lobby", Description: "d", Instructions: "i", Audience: AudiencePublic},
	}, nil, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for id, want := range map[string]string{"crew": GroupContextShared, "lobby": GroupContextPerPrincipal, DefaultID: GroupContextPerPrincipal} {
		definition, ok := registry.Lookup(id)
		if !ok || definition.GroupContext != want {
			t.Fatalf("%s group context = %q (found %v), want %q", id, definition.GroupContext, ok, want)
		}
	}
}
