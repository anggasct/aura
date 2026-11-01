package agent

import (
	"slices"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
)

func kantorOverride() config.AgentDefinition {
	return config.AgentDefinition{
		ID:              "kantor",
		Name:            "Kantor",
		Description:     "Office assistant",
		Instructions:    "Help the team.",
		Audience:        config.AudienceTrusted,
		Tools:           []string{"web_search", "web_fetch", "read_file", "write_file"},
		PrincipalTools:  []string{"web_search", "write_file"},
		PublicTools:     []string{"web_search"},
		ApprovalTimeout: config.Duration(10 * time.Minute),
		Messages:        map[string]string{config.MessageWaitingForOwner: "Sebentar ya."},
	}
}

func TestBuildCarriesAccessFields(t *testing.T) {
	registry, err := Build([]config.AgentDefinition{kantorOverride()}, validTools, []string{"primary"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	kantor, _ := registry.Lookup("kantor")
	if kantor.Audience != config.AudienceTrusted || kantor.ApprovalTimeout != 10*time.Minute {
		t.Fatalf("kantor access = %q %v", kantor.Audience, kantor.ApprovalTimeout)
	}
	if got := kantor.Message(config.MessageWaitingForOwner); got != "Sebentar ya." {
		t.Errorf("override message = %q", got)
	}
	if got, want := kantor.Message(config.MessageApprovalTimeout), config.PersonaMessageDefaults()[config.MessageApprovalTimeout]; got != want {
		t.Errorf("default message = %q, want %q", got, want)
	}
	for _, id := range []string{"main", "engineer", "reviewer", "researcher"} {
		builtin, _ := registry.Lookup(id)
		if builtin.Audience != config.AudienceOwner {
			t.Errorf("%s audience = %q, want owner by default", id, builtin.Audience)
		}
	}
	custom := kantorOverride()
	custom.Audience = ""
	registry, err = Build([]config.AgentDefinition{custom}, validTools, []string{"primary"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if defaulted, _ := registry.Lookup("kantor"); defaulted.Audience != config.AudienceOwner {
		t.Errorf("unset audience = %q, want owner", defaulted.Audience)
	}
}

func TestDefinitionAdmitsByAudience(t *testing.T) {
	classes := []principal.Class{principal.ClassOwner, principal.ClassTrusted, principal.ClassPublic, ""}
	want := map[string][]bool{
		config.AudienceOwner:   {true, false, false, false},
		config.AudienceTrusted: {true, true, false, true},
		config.AudiencePublic:  {true, true, true, true},
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
	registry, err := Build([]config.AgentDefinition{kantorOverride()}, validTools, []string{"primary"})
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
	cases := map[string]func(*config.AgentDefinition){
		"public effect tool":           func(d *config.AgentDefinition) { d.PublicTools = []string{"write_file"} },
		"principal tool outside tools": func(d *config.AgentDefinition) { d.PrincipalTools = []string{"exec"} },
		"public tool outside tools":    func(d *config.AgentDefinition) { d.PublicTools = []string{"list_dir"} },
		"public exec without tools": func(d *config.AgentDefinition) {
			d.Tools = nil
			d.PublicTools = []string{"exec"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			override := kantorOverride()
			mutate(&override)
			_, err := Build([]config.AgentDefinition{override}, validTools, []string{"primary"})
			if code, ok := CodeOf(err); !ok || code != ErrorCodePersonaInvalid {
				t.Fatalf("Build() code = %q, %v (err %v), want persona_invalid", code, ok, err)
			}
		})
	}
	unrestricted := kantorOverride()
	unrestricted.Tools = nil
	unrestricted.PrincipalTools = []string{"exec"}
	unrestricted.PublicTools = []string{"list_dir"}
	if _, err := Build([]config.AgentDefinition{unrestricted}, validTools, []string{"primary"}); err != nil {
		t.Fatalf("persona without a tools list accepts any read-tier ceiling: %v", err)
	}
}

func TestRouterServesPublicOnlyWhenAPublicPersonaIsReachable(t *testing.T) {
	public := kantorOverride()
	public.ID = "lobby"
	public.Audience = config.AudiencePublic
	registry, err := Build([]config.AgentDefinition{kantorOverride(), public}, validTools, []string{"primary"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	discord := &config.Discord{Accounts: []config.DiscordAccount{
		{Name: "home", Persona: DefaultID},
		{Name: "office", Persona: "kantor"},
		{Name: "front", Persona: "lobby"},
	}}
	router, err := NewRouter(registry, discord, []config.Binding{discordBinding("lobby", "home", config.Binding{ChannelID: "9"})})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	for account, want := range map[string]bool{"home": true, "office": false, "front": true, "missing": false} {
		if got := router.ServesPublic(config.BindingChannelDiscord, account); got != want {
			t.Errorf("ServesPublic(%s) = %v, want %v", account, got, want)
		}
	}
	if definition, ok := router.Persona("kantor"); !ok || definition.Name != "Kantor" {
		t.Errorf("Persona(kantor) = %+v, %v", definition, ok)
	}
}
