package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/anggasct/aura/internal/channels/discord"
	legacyconfig "github.com/anggasct/aura/internal/config"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/secret"
	platformwebhook "github.com/anggasct/aura/internal/platform/webhook"
)

func validateReferences(cfg *Config, doc *document) error {
	return errors.Join(
		modelRouteReferenceProblems(cfg.ModelRoutes, cfg.Models),
		validateRouting(cfg.Models, doc),
		webhookListenProblem(&cfg.Webhook, cfg.Health.Listen),
		errors.Join(telegramDiscordTokenProblems(&cfg.Channels.Telegram, &cfg.Channels.Discord)...),
		telegramOwnerProblem(&cfg.Channels.Telegram, cfg.Owner),
		emailOwnerProblem(&cfg.Channels.Email, cfg.Owner),
		errors.Join(bindingAccountProblems(cfg.Bindings, &cfg.Channels)...),
		screenOwnerSecrets(cfg.Owner, cfg),
	)
}

func modelRouteReferenceProblems(routes model.Routes, models model.Config) error {
	if len(models.Definitions) == 0 {
		return nil
	}
	var problems []error
	for _, name := range slices.Sorted(maps.Keys(routes)) {
		for _, candidate := range routes[name].Candidates {
			if _, ok := models.Definitions[candidate]; !ok {
				problems = append(problems, configkit.Invalid("model_routes."+name, "references unknown model definition"))
			}
		}
	}
	return errors.Join(problems...)
}

func webhookListenProblem(webhook *platformwebhook.Config, healthListen string) error {
	if webhook == nil {
		return configkit.Invalid("webhook", "must not be nil")
	}
	if webhook.Enabled && webhook.Listen == healthListen {
		return configkit.Invalid("webhook.listen_address", "must differ from health.listen")
	}
	return nil
}

func telegramDiscordTokenProblems(telegram *legacyconfig.Telegram, discordCfg *discord.Config) []error {
	if telegram == nil {
		return []error{configkit.Invalid("channels.telegram", "must not be nil")}
	}
	if discordCfg == nil {
		return []error{configkit.Invalid("channels.discord", "must not be nil")}
	}
	discordRefs := make(map[secret.Ref]bool, len(discordCfg.Accounts))
	for i := range discordCfg.Accounts {
		discordRefs[discordCfg.Accounts[i].BotTokenRef] = true
	}
	var problems []error
	telegramRefs := make(map[secret.Ref]bool, len(telegram.Accounts))
	for i := range telegram.Accounts {
		ref := telegram.Accounts[i].BotTokenRef
		if ref.IsZero() {
			continue
		}
		if discordRefs[ref] && !telegramRefs[ref] {
			problems = append(problems, configkit.Invalid(fmt.Sprintf("channels.telegram.accounts[%d].bot_token_ref", i), "is shared with another account"))
		}
		telegramRefs[ref] = true
	}
	for i := range telegram.Accounts {
		account := &telegram.Accounts[i]
		ref := account.WebhookSecretRef
		if account.Mode == legacyconfig.TelegramModeWebhook && !ref.IsZero() && discordRefs[ref] && !telegramRefs[ref] {
			problems = append(problems, configkit.Invalid(fmt.Sprintf("channels.telegram.accounts[%d].webhook_secret_ref", i), "must differ from every bot token reference"))
		}
	}
	return problems
}

func telegramOwnerProblem(telegram *legacyconfig.Telegram, owner *principal.Owner) error {
	if telegram == nil {
		return configkit.Invalid("channels.telegram", "must not be nil")
	}
	if !telegram.Enabled || hasOwnerChannel(owner, principal.ChannelTelegram) {
		return nil
	}
	for i := range telegram.Accounts {
		if len(telegram.Accounts[i].AllowedUserIDs) == 0 {
			return configkit.Invalid("channels.telegram.enabled", "requires a telegram:<user_id> entry in owner.identities or an allowed_user_ids entry on every account")
		}
	}
	return nil
}

func emailOwnerProblem(email *legacyconfig.Email, owner *principal.Owner) error {
	if email == nil {
		return configkit.Invalid("channels.email", "must not be nil")
	}
	if !email.Enabled || hasOwnerChannel(owner, principal.ChannelEmail) {
		return nil
	}
	for i := range email.Accounts {
		if len(email.Accounts[i].AllowedSenders) == 0 {
			return configkit.Invalid("channels.email.enabled", "requires an email:<address> entry in owner.identities or an allowed_senders entry on every account")
		}
	}
	return nil
}

func hasOwnerChannel(owner *principal.Owner, channel string) bool {
	if owner == nil {
		return false
	}
	for _, identity := range owner.Identities {
		if identityChannel, _, found := strings.Cut(identity, ":"); found && identityChannel == channel {
			return true
		}
	}
	return false
}

func bindingAccountProblems(bindings auraagent.Bindings, channels *Channels) []error {
	if channels == nil {
		return []error{configkit.Invalid("channels", "must not be nil")}
	}
	accounts := map[string]map[string]bool{
		auraagent.BindingChannelDiscord:  make(map[string]bool, len(channels.Discord.Accounts)),
		auraagent.BindingChannelTelegram: make(map[string]bool, len(channels.Telegram.Accounts)),
	}
	for i := range channels.Discord.Accounts {
		accounts[auraagent.BindingChannelDiscord][channels.Discord.Accounts[i].Name] = true
	}
	for i := range channels.Telegram.Accounts {
		accounts[auraagent.BindingChannelTelegram][channels.Telegram.Accounts[i].Name] = true
	}
	var problems []error
	for i := range bindings {
		names, supported := accounts[bindings[i].Channel]
		if supported && !names[bindings[i].Account] {
			problems = append(problems, configkit.Field(fmt.Sprintf("bindings[%d].account", i), fault.New(auraagent.ErrorCodeBindingInvalid, fmt.Sprintf("%q is not a configured channels.%s.accounts name", bindings[i].Account, bindings[i].Channel))))
		}
	}
	return problems
}
