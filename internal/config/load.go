package config

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/secret"
)

func ValidateTerminal(t Terminal) error {
	if t.RenderHz <= 0 {
		return configkit.Invalid("terminal.render_hz", "must be positive")
	}
	if t.MaxInputBytes <= 0 {
		return configkit.Invalid("terminal.max_input_bytes", "must be positive")
	}
	if t.InMemoryHistory < 0 {
		return configkit.Invalid("terminal.in_memory_history", "must not be negative")
	}
	if t.SecondInterruptTime <= 0 {
		return configkit.Invalid("terminal.second_interrupt_window", "must be positive")
	}
	switch t.PlainApproval {
	case "deny":
	default:
		return configkit.Invalid("terminal.plain_approval", "must be \"deny\"")
	}
	return nil
}

const (
	telegramMaxPollTimeout = 50 * time.Second
	telegramMaxBackoffCap  = 10 * time.Minute
	telegramMinEditFloor   = time.Second
	telegramMinEditCeiling = time.Minute
)

func ValidateTelegram(telegram *Telegram) error {
	if telegram == nil {
		return configkit.Invalid("channels.telegram", "must not be nil")
	}
	var problems []error
	names := make(map[string]bool, len(telegram.Accounts))
	tokenRefs := make(map[secret.Ref]bool, len(telegram.Accounts))
	for i := range telegram.Accounts {
		account := &telegram.Accounts[i]
		field := fmt.Sprintf("channels.telegram.accounts[%d]", i)
		if err := configkit.ValidateAccountName(field+".name", account.Name); err != nil {
			problems = append(problems, err)
		} else if names[account.Name] {
			problems = append(problems, configkit.Invalid(field+".name", fmt.Sprintf("%q is configured more than once", account.Name)))
		} else {
			names[account.Name] = true
		}
		switch {
		case account.BotTokenRef.IsZero():
			problems = append(problems, configkit.Invalid(field+".bot_token_ref", "must not be empty"))
		case tokenRefs[account.BotTokenRef]:
			problems = append(problems, configkit.Invalid(field+".bot_token_ref", "is shared with another account"))
		default:
			tokenRefs[account.BotTokenRef] = true
		}
		problems = append(problems, telegramUserListProblems(field+".allowed_user_ids", account.AllowedUserIDs)...)
		problems = append(problems, telegramChatListProblems(field+".allowed_chat_ids", account.AllowedChatIDs)...)
	}
	for i := range telegram.Accounts {
		problems = append(problems, telegramModeProblems(fmt.Sprintf("channels.telegram.accounts[%d]", i), &telegram.Accounts[i], tokenRefs)...)
	}
	problems = append(problems, telegramWebhookProblems(telegram)...)
	if telegram.Enabled && len(telegram.Accounts) == 0 {
		problems = append(problems, configkit.Invalid("channels.telegram.enabled", "requires at least one channels.telegram.accounts entry"))
	}
	timeout := time.Duration(telegram.Polling.Timeout)
	if timeout < time.Second || timeout > telegramMaxPollTimeout || timeout%time.Second != 0 {
		problems = append(problems, configkit.Invalid("channels.telegram.polling.timeout", fmt.Sprintf("must be a whole number of seconds between 1s and %s", telegramMaxPollTimeout)))
	}
	backoff := time.Duration(telegram.Polling.MaxBackoff)
	if backoff < time.Second || backoff > telegramMaxBackoffCap {
		problems = append(problems, configkit.Invalid("channels.telegram.polling.max_backoff", fmt.Sprintf("must be between 1s and %s", telegramMaxBackoffCap)))
	}
	editInterval := time.Duration(telegram.MinEditInterval)
	if editInterval < telegramMinEditFloor || editInterval > telegramMinEditCeiling {
		problems = append(problems, configkit.Invalid("channels.telegram.min_edit_interval", fmt.Sprintf("must be between %s and %s", telegramMinEditFloor, telegramMinEditCeiling)))
	}
	if telegram.MaxAttachmentBytes < 1 || telegram.MaxAttachmentBytes > TelegramMaxAttachmentBytes {
		problems = append(problems, configkit.Invalid("channels.telegram.max_attachment_bytes", fmt.Sprintf("must be between 1 and %d", TelegramMaxAttachmentBytes)))
	}
	return errors.Join(problems...)
}

func ValidateHTTP(http *HTTP) error {
	var problems []error
	if err := configkit.ValidateListenAddress("channels.http.listen_address", http.ListenAddress); err != nil {
		problems = append(problems, err)
	}
	if http.MaxBodyBytes < 1 || http.MaxBodyBytes > HTTPMaxBodyCeiling {
		problems = append(problems, configkit.Invalid("channels.http.max_body_bytes", fmt.Sprintf("must be between 1 and %d", HTTPMaxBodyCeiling)))
	}
	if http.RequestsPerMinute < 1 || http.RequestsPerMinute > HTTPRequestsPerMinuteCeiling {
		problems = append(problems, configkit.Invalid("channels.http.requests_per_minute", fmt.Sprintf("must be between 1 and %d", HTTPRequestsPerMinuteCeiling)))
	}
	if http.MaxConcurrentPerKey < 1 || http.MaxConcurrentPerKey > HTTPMaxConcurrentPerKeyLimit {
		problems = append(problems, configkit.Invalid("channels.http.max_concurrent_per_key", fmt.Sprintf("must be between 1 and %d", HTTPMaxConcurrentPerKeyLimit)))
	}
	if http.MaxHistoryMessages < 0 || http.MaxHistoryMessages > HTTPMaxHistoryMessagesLimit {
		problems = append(problems, configkit.Invalid("channels.http.max_history_messages", fmt.Sprintf("must be between 0 and %d", HTTPMaxHistoryMessagesLimit)))
	}
	if http.MaxHistoryBytes < 0 || http.MaxHistoryBytes > HTTPMaxHistoryBytesLimit {
		problems = append(problems, configkit.Invalid("channels.http.max_history_bytes", fmt.Sprintf("must be between 0 and %d", HTTPMaxHistoryBytesLimit)))
	}
	if keepalive := time.Duration(http.KeepaliveInterval); keepalive < HTTPKeepaliveIntervalFloor || keepalive > HTTPKeepaliveIntervalCeiling {
		problems = append(problems, configkit.Invalid("channels.http.keepalive_interval", fmt.Sprintf("must be between %s and %s", HTTPKeepaliveIntervalFloor, HTTPKeepaliveIntervalCeiling)))
	}
	return errors.Join(problems...)
}

func telegramModeProblems(field string, account *TelegramAccount, tokenRefs map[secret.Ref]bool) []error {
	switch account.Mode {
	case TelegramModePolling:
		if !account.WebhookSecretRef.IsZero() {
			return []error{configkit.Invalid(field+".webhook_secret_ref", "is only valid when mode is "+TelegramModeWebhook)}
		}
		return nil
	case TelegramModeWebhook:
		switch {
		case account.WebhookSecretRef.IsZero():
			return []error{configkit.Invalid(field+".webhook_secret_ref", "must not be empty in webhook mode")}
		case tokenRefs[account.WebhookSecretRef]:
			return []error{configkit.Invalid(field+".webhook_secret_ref", "must differ from every bot token reference")}
		}
		return nil
	default:
		return []error{configkit.Invalid(field+".mode", fmt.Sprintf("%q must be %s or %s", account.Mode, TelegramModePolling, TelegramModeWebhook))}
	}
}

func telegramWebhookProblems(telegram *Telegram) []error {
	var problems []error
	webhook := &telegram.Webhook
	if err := configkit.ValidateListenAddress("channels.telegram.webhook.listen_address", webhook.ListenAddress); err != nil {
		problems = append(problems, err)
	}
	if webhook.MaxBodyBytes < 1 || webhook.MaxBodyBytes > TelegramWebhookMaxBodyCeiling {
		problems = append(problems, configkit.Invalid("channels.telegram.webhook.max_body_bytes", fmt.Sprintf("must be between 1 and %d", TelegramWebhookMaxBodyCeiling)))
	}
	if webhook.MaxConnections < 1 || webhook.MaxConnections > TelegramWebhookConnectionLimit {
		problems = append(problems, configkit.Invalid("channels.telegram.webhook.max_connections", fmt.Sprintf("must be between 1 and %d", TelegramWebhookConnectionLimit)))
	}
	if webhook.PublicBaseURL == "" {
		if telegram.UsesWebhook() {
			problems = append(problems, configkit.Invalid(telegramWebhookPublicBaseURLField, "is required when an account uses webhook mode"))
		}
		return problems
	}
	if err := validatePublicWebhookURL(webhook.PublicBaseURL); err != nil {
		problems = append(problems, err)
	}
	return problems
}

const telegramWebhookPublicBaseURLField = "channels.telegram.webhook.public_base_url"

func validatePublicWebhookURL(raw string) error {
	if strings.ContainsAny(raw, "?#") {
		return configkit.Invalid(telegramWebhookPublicBaseURLField, "must not carry a query or fragment")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return configkit.Invalid(telegramWebhookPublicBaseURLField, "is not a valid URL")
	}
	if u.Scheme != "https" {
		return configkit.Invalid(telegramWebhookPublicBaseURLField, "must use https")
	}
	if err := egress.ValidateBaseURL(raw); err != nil {
		return configkit.Invalid(telegramWebhookPublicBaseURLField, "is not valid: "+err.Error())
	}
	return nil
}

func telegramUserListProblems(field string, ids []string) []error {
	var problems []error
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		switch {
		case id == "":
			problems = append(problems, configkit.Invalid(field, "must not contain empty ids"))
		case !principal.IsTelegramUserID(id):
			problems = append(problems, configkit.Invalid(field, fmt.Sprintf("id %q is not a positive decimal telegram user id", id)))
		case seen[id]:
			problems = append(problems, configkit.Invalid(field, fmt.Sprintf("id %q is configured more than once", id)))
		default:
			seen[id] = true
		}
	}
	return problems
}

func telegramChatListProblems(field string, ids []string) []error {
	var problems []error
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		switch {
		case id == "":
			problems = append(problems, configkit.Invalid(field, "must not contain empty ids"))
		case !principal.IsTelegramGroupChatID(id):
			problems = append(problems, configkit.Invalid(field, fmt.Sprintf("id %q is not a negative decimal telegram group chat id", id)))
		case seen[id]:
			problems = append(problems, configkit.Invalid(field, fmt.Sprintf("id %q is configured more than once", id)))
		default:
			seen[id] = true
		}
	}
	return problems
}

func ValidateTools(toolsConfig *Tools) error {
	if toolsConfig == nil {
		return nil
	}
	if strings.TrimSpace(toolsConfig.Workspace) == "" {
		return configkit.Invalid("tools.workspace", "must not be empty")
	}
	if !filepath.IsAbs(toolsConfig.Workspace) {
		return configkit.Invalid("tools.workspace", "must be an absolute path")
	}
	if toolsConfig.MaxInlineResultBytes <= 0 {
		return configkit.Invalid("tools.max_inline_result_bytes", "must be positive")
	}
	if err := configkit.Under("tools.exec", toolsConfig.Exec.Validate()); err != nil {
		return err
	}
	if err := configkit.Under("tools.fetch", toolsConfig.Fetch.Validate()); err != nil {
		return err
	}
	if err := configkit.Under("tools.github", toolsConfig.GitHub.Validate()); err != nil {
		return err
	}
	return configkit.Under("tools.web_search", toolsConfig.WebSearch.Validate())
}
