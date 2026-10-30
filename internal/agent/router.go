package agent

import (
	"errors"
	"fmt"

	"github.com/anggasct/aura/internal/config"
)

const (
	ScoreUser           = 16
	ScoreThread         = 8
	ScoreChannel        = 4
	ScoreGuild          = 2
	ScoreAccountDefault = 1
)

const AccountDefaultBinding = -1

type RouteScope struct {
	Channel   string
	Account   string
	GuildID   string
	ChannelID string
	ThreadID  string
	UserID    string
}

type RouteMatch struct {
	Persona string
	Score   int
	Binding int
}

type routeAccount struct {
	channel string
	account string
}

type Router struct {
	defaults map[routeAccount]string
	accounts map[routeAccount]bool
	bindings []config.Binding
	personas []string
}

func NewRouter(registry *Registry, discord *config.Discord, bindings []config.Binding) (*Router, error) {
	if registry == nil {
		return nil, codedError(ErrorCodeBindingInvalid, "agent registry must not be nil")
	}
	if discord == nil {
		return nil, codedError(ErrorCodeBindingInvalid, "discord config must not be nil")
	}
	router := &Router{
		defaults: make(map[routeAccount]string, len(discord.Accounts)),
		accounts: make(map[routeAccount]bool, len(discord.Accounts)),
		bindings: append([]config.Binding(nil), bindings...),
	}
	for i := range registry.definitions {
		router.personas = append(router.personas, registry.definitions[i].ID)
	}
	var problems []error
	for i := range discord.Accounts {
		account := &discord.Accounts[i]
		key := routeAccount{channel: config.BindingChannelDiscord, account: account.Name}
		router.accounts[key] = true
		if account.Persona == "" {
			continue
		}
		if _, ok := registry.Lookup(account.Persona); !ok {
			problems = append(problems, fmt.Errorf("channels.discord.accounts[%d].persona %q is not a registered persona", i, account.Persona))
			continue
		}
		router.defaults[key] = account.Persona
	}
	for i := range bindings {
		binding := &bindings[i]
		if _, ok := registry.Lookup(binding.Persona); !ok {
			problems = append(problems, fmt.Errorf("bindings[%d].persona %q is not a registered persona", i, binding.Persona))
		}
	}
	if err := errors.Join(problems...); err != nil {
		return nil, codedError(ErrorCodeBindingInvalid, err.Error())
	}
	return router, nil
}

func (r *Router) HasAccount(channel, account string) bool {
	return r != nil && r.accounts[routeAccount{channel: channel, account: account}]
}

func (r *Router) Personas() []string {
	if r == nil {
		return nil
	}
	return append([]string(nil), r.personas...)
}

func (r *Router) Resolve(scope *RouteScope) (RouteMatch, bool) {
	if r == nil || scope == nil {
		return RouteMatch{}, false
	}
	best := RouteMatch{Binding: AccountDefaultBinding}
	for i := range r.bindings {
		binding := &r.bindings[i]
		if binding.Channel != scope.Channel || binding.Account != scope.Account {
			continue
		}
		score, ok := bindingScore(binding, scope)
		if !ok || score <= best.Score {
			continue
		}
		best = RouteMatch{Persona: binding.Persona, Score: score, Binding: i}
	}
	if best.Persona != "" {
		return best, true
	}
	persona, ok := r.defaults[routeAccount{channel: scope.Channel, account: scope.Account}]
	if !ok {
		return RouteMatch{}, false
	}
	return RouteMatch{Persona: persona, Score: ScoreAccountDefault, Binding: AccountDefaultBinding}, true
}

func bindingScore(binding *config.Binding, scope *RouteScope) (int, bool) {
	score := 0
	for _, field := range []struct {
		want, got string
		weight    int
	}{
		{binding.UserID, scope.UserID, ScoreUser},
		{binding.ThreadID, scope.ThreadID, ScoreThread},
		{binding.ChannelID, scope.ChannelID, ScoreChannel},
		{binding.GuildID, scope.GuildID, ScoreGuild},
	} {
		if field.want == "" {
			continue
		}
		if field.want != field.got {
			return 0, false
		}
		score += field.weight
	}
	return score, score > 0
}
