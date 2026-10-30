package agent

import (
	"testing"

	"github.com/anggasct/aura/internal/config"
)

func routerFixture(t *testing.T, bindings []config.Binding) *Router {
	t.Helper()
	registry, err := Build(nil, validTools, []string{"primary"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	discord := &config.Discord{Accounts: []config.DiscordAccount{
		{Name: "home", Persona: DefaultID},
		{Name: "kuma"},
	}}
	router, err := NewRouter(registry, discord, bindings)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return router
}

func discordBinding(persona, account string, match config.Binding) config.Binding {
	match.Persona = persona
	match.Channel = config.BindingChannelDiscord
	match.Account = account
	return match
}

func TestRouterMostSpecificBindingWins(t *testing.T) {
	router := routerFixture(t, []config.Binding{
		discordBinding("researcher", "home", config.Binding{GuildID: "1"}),
		discordBinding("engineer", "home", config.Binding{ChannelID: "2"}),
		discordBinding("reviewer", "home", config.Binding{ThreadID: "3"}),
		discordBinding("researcher", "home", config.Binding{UserID: "4"}),
		discordBinding("reviewer", "home", config.Binding{GuildID: "1", ChannelID: "5"}),
		discordBinding("engineer", "kuma", config.Binding{ChannelID: "2"}),
	})
	cases := []struct {
		name        string
		scope       RouteScope
		wantPersona string
		wantScore   int
		wantBinding int
	}{
		{"user beats thread channel guild", RouteScope{GuildID: "1", ChannelID: "2", ThreadID: "3", UserID: "4"}, "researcher", ScoreUser, 3},
		{"thread beats channel and guild", RouteScope{GuildID: "1", ChannelID: "2", ThreadID: "3", UserID: "9"}, "reviewer", ScoreThread, 2},
		{"channel beats guild", RouteScope{GuildID: "1", ChannelID: "2", UserID: "9"}, "engineer", ScoreChannel, 1},
		{"guild and channel beat channel alone", RouteScope{GuildID: "1", ChannelID: "5", UserID: "9"}, "reviewer", ScoreGuild + ScoreChannel, 4},
		{"guild beats account default", RouteScope{GuildID: "1", ChannelID: "7", UserID: "9"}, "researcher", ScoreGuild, 0},
		{"account default", RouteScope{GuildID: "8", ChannelID: "7", UserID: "9"}, DefaultID, ScoreAccountDefault, AccountDefaultBinding},
		{"dm falls back to account default", RouteScope{ChannelID: "6", UserID: "9"}, DefaultID, ScoreAccountDefault, AccountDefaultBinding},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope := tc.scope
			scope.Channel = config.BindingChannelDiscord
			scope.Account = "home"
			match, ok := router.Resolve(&scope)
			if !ok {
				t.Fatal("no match")
			}
			if match.Persona != tc.wantPersona || match.Score != tc.wantScore || match.Binding != tc.wantBinding {
				t.Errorf("match = %+v, want persona %q score %d binding %d", match, tc.wantPersona, tc.wantScore, tc.wantBinding)
			}
		})
	}
}

func TestRouterScopesBindingsToTheirAccount(t *testing.T) {
	router := routerFixture(t, []config.Binding{
		discordBinding("engineer", "kuma", config.Binding{ChannelID: "2"}),
	})
	home, ok := router.Resolve(&RouteScope{Channel: config.BindingChannelDiscord, Account: "home", ChannelID: "2"})
	if !ok || home.Persona != DefaultID {
		t.Errorf("home = %+v, %t; want account default %q", home, ok, DefaultID)
	}
	kuma, ok := router.Resolve(&RouteScope{Channel: config.BindingChannelDiscord, Account: "kuma", ChannelID: "2"})
	if !ok || kuma.Persona != "engineer" || kuma.Score != ScoreChannel {
		t.Errorf("kuma = %+v, %t; want engineer by channel", kuma, ok)
	}
}

func TestRouterNoMatchWithoutAccountDefault(t *testing.T) {
	router := routerFixture(t, nil)
	if match, ok := router.Resolve(&RouteScope{Channel: config.BindingChannelDiscord, Account: "kuma", ChannelID: "2"}); ok {
		t.Fatalf("match = %+v, want none", match)
	}
	if match, ok := router.Resolve(&RouteScope{Channel: config.BindingChannelDiscord, Account: "work", ChannelID: "2"}); ok {
		t.Fatalf("unknown account match = %+v, want none", match)
	}
	if _, ok := router.Resolve(nil); ok {
		t.Fatal("nil scope matched")
	}
}

func TestRouterRejectsUnregisteredPersonas(t *testing.T) {
	registry, err := Build(nil, validTools, []string{"primary"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	cases := map[string]struct {
		discord  config.Discord
		bindings []config.Binding
	}{
		"binding persona": {
			discord:  config.Discord{Accounts: []config.DiscordAccount{{Name: "home"}}},
			bindings: []config.Binding{discordBinding("ghost", "home", config.Binding{ChannelID: "2"})},
		},
		"account default persona": {
			discord: config.Discord{Accounts: []config.DiscordAccount{{Name: "home", Persona: "ghost"}}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewRouter(registry, &tc.discord, tc.bindings)
			if code, ok := CodeOf(err); !ok || code != ErrorCodeBindingInvalid {
				t.Fatalf("NewRouter error = %v, want %s", err, ErrorCodeBindingInvalid)
			}
		})
	}
	if _, err := NewRouter(nil, &config.Discord{}, nil); err == nil {
		t.Error("nil registry accepted")
	}
	if _, err := NewRouter(registry, nil, nil); err == nil {
		t.Error("nil discord config accepted")
	}
}

func TestRouterListsRegisteredPersonasAndAccounts(t *testing.T) {
	router := routerFixture(t, nil)
	personas := router.Personas()
	if len(personas) != 4 || personas[0] != DefaultID {
		t.Errorf("personas = %v", personas)
	}
	if !router.HasAccount(config.BindingChannelDiscord, "kuma") || router.HasAccount(config.BindingChannelDiscord, "work") {
		t.Error("HasAccount does not reflect configured accounts")
	}
}
