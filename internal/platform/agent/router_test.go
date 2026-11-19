package agent

import (
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
)

func routerFixture(t *testing.T, bindings []Binding) *Router {
	t.Helper()
	registry, err := Build(nil, validTools, []string{"primary"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	router, err := NewRouter(registry, []ChannelAccount{
		{Channel: BindingChannelDiscord, Name: "home", Persona: DefaultID, Field: "channels.discord.accounts[0]"},
		{Channel: BindingChannelDiscord, Name: "kuma", Field: "channels.discord.accounts[1]"},
		{Channel: BindingChannelTelegram, Name: "pocket", Persona: "researcher", Field: "channels.telegram.accounts[0]"},
	}, bindings)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return router
}

func discordBinding(persona, account string, match Binding) Binding {
	match.Persona = persona
	match.Channel = BindingChannelDiscord
	match.Account = account
	return match
}

func TestRouterMostSpecificBindingWins(t *testing.T) {
	router := routerFixture(t, []Binding{
		discordBinding("researcher", "home", Binding{GuildID: "1"}),
		discordBinding("engineer", "home", Binding{ChannelID: "2"}),
		discordBinding("reviewer", "home", Binding{ThreadID: "3"}),
		discordBinding("researcher", "home", Binding{UserID: "4"}),
		discordBinding("reviewer", "home", Binding{GuildID: "1", ChannelID: "5"}),
		discordBinding("engineer", "kuma", Binding{ChannelID: "2"}),
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
			scope.Channel = BindingChannelDiscord
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
	router := routerFixture(t, []Binding{
		discordBinding("engineer", "kuma", Binding{ChannelID: "2"}),
	})
	home, ok := router.Resolve(&RouteScope{Channel: BindingChannelDiscord, Account: "home", ChannelID: "2"})
	if !ok || home.Persona != DefaultID {
		t.Errorf("home = %+v, %t; want account default %q", home, ok, DefaultID)
	}
	kuma, ok := router.Resolve(&RouteScope{Channel: BindingChannelDiscord, Account: "kuma", ChannelID: "2"})
	if !ok || kuma.Persona != "engineer" || kuma.Score != ScoreChannel {
		t.Errorf("kuma = %+v, %t; want engineer by channel", kuma, ok)
	}
}

func TestRouterNoMatchWithoutAccountDefault(t *testing.T) {
	router := routerFixture(t, nil)
	if match, ok := router.Resolve(&RouteScope{Channel: BindingChannelDiscord, Account: "kuma", ChannelID: "2"}); ok {
		t.Fatalf("match = %+v, want none", match)
	}
	if match, ok := router.Resolve(&RouteScope{Channel: BindingChannelDiscord, Account: "work", ChannelID: "2"}); ok {
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
		accounts []ChannelAccount
		bindings []Binding
	}{
		"binding persona": {
			accounts: []ChannelAccount{{Channel: BindingChannelDiscord, Name: "home", Field: "channels.discord.accounts[0]"}},
			bindings: []Binding{discordBinding("ghost", "home", Binding{ChannelID: "2"})},
		},
		"account default persona": {
			accounts: []ChannelAccount{{Channel: BindingChannelDiscord, Name: "home", Persona: "ghost", Field: "channels.discord.accounts[0]"}},
		},
		"telegram account default persona": {
			accounts: []ChannelAccount{{Channel: BindingChannelTelegram, Name: "pocket", Persona: "ghost", Field: "channels.telegram.accounts[0]"}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewRouter(registry, tc.accounts, tc.bindings)
			if !fault.HasCode(err, ErrorCodeBindingInvalid) {
				t.Fatalf("NewRouter error = %v, want %s", err, ErrorCodeBindingInvalid)
			}
		})
	}
	if _, err := NewRouter(nil, nil, nil); err == nil {
		t.Error("nil registry accepted")
	}
}

func TestRouterListsRegisteredPersonasAndAccounts(t *testing.T) {
	router := routerFixture(t, nil)
	personas := router.Personas()
	if len(personas) != 4 || personas[0] != DefaultID {
		t.Errorf("personas = %v", personas)
	}
	if !router.HasAccount(BindingChannelDiscord, "kuma") || router.HasAccount(BindingChannelDiscord, "work") {
		t.Error("HasAccount does not reflect configured accounts")
	}
	if !router.HasAccount(BindingChannelTelegram, "pocket") || router.HasAccount(BindingChannelDiscord, "pocket") {
		t.Error("HasAccount does not keep telegram accounts apart from discord")
	}
}

func TestRouterResolvesTelegramAccountsAndUserBindings(t *testing.T) {
	router := routerFixture(t, []Binding{{
		Persona: "engineer",
		Channel: BindingChannelTelegram,
		Account: "pocket",
		UserID:  "42",
	}})
	match, ok := router.Resolve(&RouteScope{Channel: BindingChannelTelegram, Account: "pocket", UserID: "7"})
	if !ok || match.Persona != "researcher" || match.Binding != AccountDefaultBinding {
		t.Fatalf("account default = %+v %v, want researcher", match, ok)
	}
	match, ok = router.Resolve(&RouteScope{Channel: BindingChannelTelegram, Account: "pocket", UserID: "42"})
	if !ok || match.Persona != "engineer" || match.Score != ScoreUser {
		t.Fatalf("user binding = %+v %v, want engineer", match, ok)
	}
	if match, ok := router.Resolve(&RouteScope{Channel: BindingChannelDiscord, Account: "pocket", UserID: "42"}); ok {
		t.Fatalf("telegram binding resolved for a discord scope: %+v", match)
	}
}

func telegramBinding(persona string, match Binding) Binding {
	match.Persona = persona
	match.Channel = BindingChannelTelegram
	match.Account = "pocket"
	return match
}

func TestRouterScoresTelegramChatBindings(t *testing.T) {
	router := routerFixture(t, []Binding{
		telegramBinding("engineer", Binding{ChatID: "-100"}),
		telegramBinding("reviewer", Binding{ChatID: "-100", ThreadID: "5"}),
		telegramBinding("researcher", Binding{ChatID: "-100", UserID: "7"}),
		telegramBinding("reviewer", Binding{UserID: "8"}),
		discordBinding("engineer", "home", Binding{ChatID: "-100"}),
	})
	cases := []struct {
		name        string
		scope       RouteScope
		wantPersona string
		wantScore   int
		wantBinding int
	}{
		{"user beats topic and chat", RouteScope{ChatID: "-100", ThreadID: "5", UserID: "7"}, "researcher", ScoreUser + ScoreChat, 2},
		{"topic beats chat", RouteScope{ChatID: "-100", ThreadID: "5", UserID: "9"}, "reviewer", ScoreThread + ScoreChat, 1},
		{"chat binding", RouteScope{ChatID: "-100", UserID: "9"}, "engineer", ScoreChat, 0},
		{"other topic falls back to the chat", RouteScope{ChatID: "-100", ThreadID: "6", UserID: "9"}, "engineer", ScoreChat, 0},
		{"user binding in any chat", RouteScope{ChatID: "-200", UserID: "8"}, "reviewer", ScoreUser, 3},
		{"unbound chat uses the account default", RouteScope{ChatID: "-200", UserID: "9"}, "researcher", ScoreAccountDefault, AccountDefaultBinding},
		{"private chat ignores chat bindings", RouteScope{UserID: "9"}, "researcher", ScoreAccountDefault, AccountDefaultBinding},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope := tc.scope
			scope.Channel, scope.Account = BindingChannelTelegram, "pocket"
			match, ok := router.Resolve(&scope)
			if !ok || match.Persona != tc.wantPersona || match.Score != tc.wantScore || match.Binding != tc.wantBinding {
				t.Fatalf("Resolve = %+v %v, want %s score %d binding %d", match, ok, tc.wantPersona, tc.wantScore, tc.wantBinding)
			}
		})
	}
}

func TestRouterChatBindingNeverMatchesDiscordScopes(t *testing.T) {
	router := routerFixture(t, []Binding{
		telegramBinding("engineer", Binding{ChatID: "-100"}),
	})
	match, ok := router.Resolve(&RouteScope{Channel: BindingChannelDiscord, Account: "home", ChannelID: "-100", ChatID: "-100"})
	if !ok || match.Binding != AccountDefaultBinding {
		t.Fatalf("Resolve = %+v %v, want the discord account default", match, ok)
	}
}

func TestRouterEmailAccountRoutesToItsPersona(t *testing.T) {
	registry, err := Build(nil, validTools, []string{"primary"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	accounts := []ChannelAccount{
		{Channel: BindingChannelEmail, Name: "work", Persona: DefaultID, Field: "channels.email.accounts[0]"},
		{Channel: BindingChannelEmail, Name: "ghost", Persona: "not-registered", Field: "channels.email.accounts[1]"},
	}
	if _, err := NewRouter(registry, accounts, nil); err == nil {
		t.Fatal("NewRouter accepted an email account with an unregistered persona")
	}
	router, err := NewRouter(registry, accounts[:1], nil)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if !router.HasAccount(BindingChannelEmail, "work") || router.HasAccount(BindingChannelTelegram, "work") {
		t.Error("email account is not registered under the email channel only")
	}
	match, ok := router.Resolve(&RouteScope{Channel: BindingChannelEmail, Account: "work", UserID: "friend@example.com"})
	if !ok || match.Persona != DefaultID || match.Binding != AccountDefaultBinding {
		t.Errorf("match = %+v %v, want the account persona", match, ok)
	}
	if !router.ServesPersona(BindingChannelEmail, "work", DefaultID) {
		t.Error("ServesPersona(email) = false for the account persona")
	}
}
