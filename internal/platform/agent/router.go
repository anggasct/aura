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
	ScoreChat           = ScoreChannel
	ScoreGuild          = 2
	ScoreAccountDefault = 1
)

const AccountDefaultBinding = -1

type RouteScope struct {
	Channel   string
	Account   string
	GuildID   string
	ChannelID string
	ChatID    string
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
	registry *Registry
}

type ChannelAccount struct {
	Channel string
	Name    string
	Persona string
	Field   string
}

func NewRouter(registry *Registry, accounts []ChannelAccount, bindings []config.Binding) (*Router, error) {
	if registry == nil {
		return nil, codedError(ErrorCodeBindingInvalid, "agent registry must not be nil")
	}
	router := &Router{
		defaults: make(map[routeAccount]string, len(accounts)),
		accounts: make(map[routeAccount]bool, len(accounts)),
		bindings: append([]config.Binding(nil), bindings...),
		registry: registry,
	}
	for i := range registry.definitions {
		router.personas = append(router.personas, registry.definitions[i].ID)
	}
	var problems []error
	for _, account := range accounts {
		key := routeAccount{channel: account.Channel, account: account.Name}
		router.accounts[key] = true
		if account.Persona == "" {
			continue
		}
		if _, ok := registry.Lookup(account.Persona); !ok {
			problems = append(problems, fmt.Errorf("%s.persona %q is not a registered persona", account.Field, account.Persona))
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

func (r *Router) Persona(id string) (Definition, bool) {
	if r == nil {
		return Definition{}, false
	}
	return r.registry.Lookup(id)
}

func (r *Router) ServesPublic(channel, account string) bool {
	if r == nil {
		return false
	}
	reachable := make([]string, 0, len(r.bindings)+1)
	if persona, ok := r.defaults[routeAccount{channel: channel, account: account}]; ok {
		reachable = append(reachable, persona)
	}
	for i := range r.bindings {
		if r.bindings[i].Channel == channel && r.bindings[i].Account == account {
			reachable = append(reachable, r.bindings[i].Persona)
		}
	}
	for _, persona := range reachable {
		if definition, ok := r.registry.Lookup(persona); ok && definition.Audience == config.AudiencePublic {
			return true
		}
	}
	return false
}

func (r *Router) ServesPersona(channel, account, persona string) bool {
	if r == nil || persona == "" {
		return false
	}
	if r.defaults[routeAccount{channel: channel, account: account}] == persona {
		return true
	}
	for i := range r.bindings {
		if r.bindings[i].Channel == channel && r.bindings[i].Account == account && r.bindings[i].Persona == persona {
			return true
		}
	}
	return false
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
		{binding.ChatID, scope.ChatID, ScoreChat},
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
