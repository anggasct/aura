package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	legacyconfig "github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/secret"
)

func validateReferences(cfg *Config, doc *document) error {
	return errors.Join(
		modelRouteReferenceProblems(cfg.ModelRoutes, cfg.Models),
		validateRouting(cfg.Models, doc),
		webhookListenProblem(&cfg.Webhook, cfg.Health.Listen),
		errors.Join(telegramDiscordTokenProblems(&cfg.Channels.Telegram, &cfg.Channels.Discord)...),
		telegramOwnerProblem(&cfg.Channels.Telegram, cfg.Owner),
		emailOwnerProblem(&cfg.Channels.Email, cfg.Owner),
		errors.Join(bindingProblems(cfg.Bindings, &cfg.Channels)...),
		validatePersonaNames(cfg),
		validatePersonaAccess(cfg),
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

func webhookListenProblem(webhook *legacyconfig.Webhook, healthListen string) error {
	if webhook == nil {
		return configkit.Invalid("webhook", "must not be nil")
	}
	if webhook.Enabled && webhook.Listen == healthListen {
		return configkit.Invalid("webhook.listen_address", "must differ from health.listen")
	}
	return nil
}

func telegramDiscordTokenProblems(telegram *legacyconfig.Telegram, discord *legacyconfig.Discord) []error {
	if telegram == nil {
		return []error{configkit.Invalid("channels.telegram", "must not be nil")}
	}
	if discord == nil {
		return []error{configkit.Invalid("channels.discord", "must not be nil")}
	}
	discordRefs := make(map[secret.Ref]bool, len(discord.Accounts))
	for i := range discord.Accounts {
		discordRefs[discord.Accounts[i].BotTokenRef] = true
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

func telegramOwnerProblem(telegram *legacyconfig.Telegram, owner *legacyconfig.Owner) error {
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

func emailOwnerProblem(email *legacyconfig.Email, owner *legacyconfig.Owner) error {
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

func hasOwnerChannel(owner *legacyconfig.Owner, channel string) bool {
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

func bindingProblems(bindings []legacyconfig.Binding, channels *legacyconfig.Channels) []error {
	if channels == nil {
		return []error{configkit.Invalid("channels", "must not be nil")}
	}
	accounts := map[string]map[string]bool{
		legacyconfig.BindingChannelDiscord:  make(map[string]bool, len(channels.Discord.Accounts)),
		legacyconfig.BindingChannelTelegram: make(map[string]bool, len(channels.Telegram.Accounts)),
	}
	for i := range channels.Discord.Accounts {
		accounts[legacyconfig.BindingChannelDiscord][channels.Discord.Accounts[i].Name] = true
	}
	for i := range channels.Telegram.Accounts {
		accounts[legacyconfig.BindingChannelTelegram][channels.Telegram.Accounts[i].Name] = true
	}
	var problems []error
	seen := make(map[legacyconfig.Binding]int, len(bindings))
	for i := range bindings {
		binding := bindings[i]
		field := fmt.Sprintf("bindings[%d]", i)
		if strings.TrimSpace(binding.Persona) == "" {
			problems = append(problems, configkit.Field(field+".persona", fault.New(legacyconfig.ErrorCodeBindingInvalid, "must not be empty")))
		}
		switch binding.Channel {
		case legacyconfig.BindingChannelDiscord:
			problems = append(problems, discordBindingProblems(field, &binding, accounts[legacyconfig.BindingChannelDiscord])...)
		case legacyconfig.BindingChannelTelegram:
			problems = append(problems, telegramBindingProblems(field, &binding, accounts[legacyconfig.BindingChannelTelegram])...)
		default:
			problems = append(problems, configkit.Field(field+".channel", fault.New(legacyconfig.ErrorCodeBindingInvalid, fmt.Sprintf("%q is not supported; use %q or %q", binding.Channel, legacyconfig.BindingChannelDiscord, legacyconfig.BindingChannelTelegram))))
		}
		key := binding
		key.Persona = ""
		if first, ok := seen[key]; ok {
			problems = append(problems, configkit.Field(field, fault.New(legacyconfig.ErrorCodeBindingInvalid, fmt.Sprintf("has the same match fields as bindings[%d]", first))))
			continue
		}
		seen[key] = i
	}
	return problems
}

func discordBindingProblems(field string, binding *legacyconfig.Binding, accounts map[string]bool) []error {
	var problems []error
	if !accounts[binding.Account] {
		problems = append(problems, configkit.Field(field+".account", fault.New(legacyconfig.ErrorCodeBindingInvalid, fmt.Sprintf("%q is not a configured channels.discord.accounts name", binding.Account))))
	}
	if binding.ChatID != "" {
		problems = append(problems, configkit.Field(field+".chat_id", fault.New(legacyconfig.ErrorCodeBindingInvalid, "is only valid on telegram bindings")))
	}
	if binding.GuildID == "" && binding.ChannelID == "" && binding.ThreadID == "" && binding.UserID == "" {
		problems = append(problems, configkit.Field(field, fault.New(legacyconfig.ErrorCodeBindingInvalid, "must set at least one of guild_id, channel_id, thread_id, user_id")))
	}
	for _, match := range []struct{ name, id string }{
		{"guild_id", binding.GuildID},
		{"channel_id", binding.ChannelID},
		{"thread_id", binding.ThreadID},
		{"user_id", binding.UserID},
	} {
		if match.id != "" && !legacyconfig.IsSnowflake(match.id) {
			problems = append(problems, configkit.Field(field+"."+match.name, fault.New(legacyconfig.ErrorCodeBindingInvalid, fmt.Sprintf("%q is not a decimal snowflake", match.id))))
		}
	}
	return problems
}

func telegramBindingProblems(field string, binding *legacyconfig.Binding, accounts map[string]bool) []error {
	var problems []error
	if !accounts[binding.Account] {
		problems = append(problems, configkit.Field(field+".account", fault.New(legacyconfig.ErrorCodeBindingInvalid, fmt.Sprintf("%q is not a configured channels.telegram.accounts name", binding.Account))))
	}
	if binding.GuildID != "" || binding.ChannelID != "" {
		problems = append(problems, configkit.Field(field, fault.New(legacyconfig.ErrorCodeBindingInvalid, "matches telegram chats by chat_id, thread_id and user_id only")))
	}
	if binding.ChatID == "" && binding.UserID == "" {
		problems = append(problems, configkit.Field(field, fault.New(legacyconfig.ErrorCodeBindingInvalid, "must set chat_id or user_id")))
	}
	if binding.ChatID != "" && !principal.IsTelegramGroupChatID(binding.ChatID) {
		problems = append(problems, configkit.Field(field+".chat_id", fault.New(legacyconfig.ErrorCodeBindingInvalid, fmt.Sprintf("%q is not a negative decimal telegram group chat id", binding.ChatID))))
	}
	if binding.ThreadID != "" {
		switch {
		case binding.ChatID == "":
			problems = append(problems, configkit.Field(field+".thread_id", fault.New(legacyconfig.ErrorCodeBindingInvalid, "requires chat_id")))
		case !principal.IsTelegramTopicID(binding.ThreadID):
			problems = append(problems, configkit.Field(field+".thread_id", fault.New(legacyconfig.ErrorCodeBindingInvalid, fmt.Sprintf("%q is not a positive decimal telegram topic id", binding.ThreadID))))
		}
	}
	if binding.UserID != "" && !principal.IsTelegramUserID(binding.UserID) {
		problems = append(problems, configkit.Field(field+".user_id", fault.New(legacyconfig.ErrorCodeBindingInvalid, fmt.Sprintf("%q is not a positive decimal telegram user id", binding.UserID))))
	}
	return problems
}
