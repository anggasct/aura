package telegram

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/secret"
)

const MaxAttachmentBytes int64 = 20971520

const (
	ModePolling = "polling"
	ModeWebhook = "webhook"
)

const (
	WebhookListenAddress   = "127.0.0.1:8283"
	WebhookMaxBodyBytes    = 1048576
	WebhookMaxBodyCeiling  = 10485760
	WebhookMaxConnections  = 4
	WebhookConnectionLimit = 100
)

const (
	maxPollTimeout            = 50 * time.Second
	maxBackoffCap             = 10 * time.Minute
	minEditFloor              = time.Second
	minEditCeiling            = time.Minute
	webhookPublicBaseURLField = "webhook.public_base_url"
)

type Config struct {
	Enabled            bool               `json:"enabled"`
	Accounts           []Account          `json:"accounts"`
	Polling            PollingConfig      `json:"polling"`
	MinEditInterval    configkit.Duration `json:"min_edit_interval"`
	MaxAttachmentBytes int64              `json:"max_attachment_bytes"`
	Webhook            WebhookConfig      `json:"webhook"`
}

type Account struct {
	Name               string     `json:"name"`
	BotTokenRef        secret.Ref `json:"bot_token_ref"`
	Mode               string     `json:"mode"`
	WebhookSecretRef   secret.Ref `json:"webhook_secret_ref,omitzero"`
	Persona            string     `json:"persona,omitempty"`
	AllowedUserIDs     []string   `json:"allowed_user_ids"`
	AllowedChatIDs     []string   `json:"allowed_chat_ids"`
	AcceptPrivateChats bool       `json:"accept_private_chats"`
	LinkPreviews       bool       `json:"link_previews"`
}

func (*Account) Default() any {
	return Account{AcceptPrivateChats: true, Mode: ModePolling}
}

func (a *Account) WebhookMode() bool {
	return a != nil && a.Mode == ModeWebhook
}

type WebhookConfig struct {
	ListenAddress  string `json:"listen_address"`
	PublicBaseURL  string `json:"public_base_url"`
	MaxBodyBytes   int64  `json:"max_body_bytes"`
	MaxConnections int    `json:"max_connections"`
}

type PollingConfig struct {
	Timeout    configkit.Duration `json:"timeout"`
	MaxBackoff configkit.Duration `json:"max_backoff"`
}

func Default() Config {
	return Config{
		Polling: PollingConfig{
			Timeout:    configkit.Duration(30 * time.Second),
			MaxBackoff: configkit.Duration(time.Minute),
		},
		MinEditInterval:    configkit.Duration(2 * time.Second),
		MaxAttachmentBytes: MaxAttachmentBytes,
		Webhook: WebhookConfig{
			ListenAddress:  WebhookListenAddress,
			MaxBodyBytes:   WebhookMaxBodyBytes,
			MaxConnections: WebhookMaxConnections,
		},
	}
}

func (c *Config) UsesWebhook() bool {
	if c == nil {
		return false
	}
	for i := range c.Accounts {
		if c.Accounts[i].WebhookMode() {
			return true
		}
	}
	return false
}

func (c *Config) Validate() error {
	if c == nil {
		return configkit.Invalid("", "must not be nil")
	}
	var problems []error
	names := make(map[string]bool, len(c.Accounts))
	tokenRefs := make(map[secret.Ref]bool, len(c.Accounts))
	for i := range c.Accounts {
		account := &c.Accounts[i]
		field := fmt.Sprintf("accounts[%d]", i)
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
		problems = append(problems, userListProblems(field+".allowed_user_ids", account.AllowedUserIDs)...)
		problems = append(problems, chatListProblems(field+".allowed_chat_ids", account.AllowedChatIDs)...)
	}
	for i := range c.Accounts {
		problems = append(problems, modeProblems(fmt.Sprintf("accounts[%d]", i), &c.Accounts[i], tokenRefs)...)
	}
	problems = append(problems, webhookProblems(c)...)
	if c.Enabled && len(c.Accounts) == 0 {
		problems = append(problems, configkit.Invalid("enabled", "requires at least one channels.telegram.accounts entry"))
	}
	timeout := time.Duration(c.Polling.Timeout)
	if timeout < time.Second || timeout > maxPollTimeout || timeout%time.Second != 0 {
		problems = append(problems, configkit.Invalid("polling.timeout", fmt.Sprintf("must be a whole number of seconds between 1s and %s", maxPollTimeout)))
	}
	backoff := time.Duration(c.Polling.MaxBackoff)
	if backoff < time.Second || backoff > maxBackoffCap {
		problems = append(problems, configkit.Invalid("polling.max_backoff", fmt.Sprintf("must be between 1s and %s", maxBackoffCap)))
	}
	editInterval := time.Duration(c.MinEditInterval)
	if editInterval < minEditFloor || editInterval > minEditCeiling {
		problems = append(problems, configkit.Invalid("min_edit_interval", fmt.Sprintf("must be between %s and %s", minEditFloor, minEditCeiling)))
	}
	if c.MaxAttachmentBytes < 1 || c.MaxAttachmentBytes > MaxAttachmentBytes {
		problems = append(problems, configkit.Invalid("max_attachment_bytes", fmt.Sprintf("must be between 1 and %d", MaxAttachmentBytes)))
	}
	return errors.Join(problems...)
}

func modeProblems(field string, account *Account, tokenRefs map[secret.Ref]bool) []error {
	switch account.Mode {
	case ModePolling:
		if !account.WebhookSecretRef.IsZero() {
			return []error{configkit.Invalid(field+".webhook_secret_ref", "is only valid when mode is "+ModeWebhook)}
		}
		return nil
	case ModeWebhook:
		switch {
		case account.WebhookSecretRef.IsZero():
			return []error{configkit.Invalid(field+".webhook_secret_ref", "must not be empty in webhook mode")}
		case tokenRefs[account.WebhookSecretRef]:
			return []error{configkit.Invalid(field+".webhook_secret_ref", "must differ from every bot token reference")}
		}
		return nil
	default:
		return []error{configkit.Invalid(field+".mode", fmt.Sprintf("%q must be %s or %s", account.Mode, ModePolling, ModeWebhook))}
	}
}

func webhookProblems(c *Config) []error {
	var problems []error
	webhook := &c.Webhook
	if err := configkit.ValidateListenAddress("webhook.listen_address", webhook.ListenAddress); err != nil {
		problems = append(problems, err)
	}
	if webhook.MaxBodyBytes < 1 || webhook.MaxBodyBytes > WebhookMaxBodyCeiling {
		problems = append(problems, configkit.Invalid("webhook.max_body_bytes", fmt.Sprintf("must be between 1 and %d", WebhookMaxBodyCeiling)))
	}
	if webhook.MaxConnections < 1 || webhook.MaxConnections > WebhookConnectionLimit {
		problems = append(problems, configkit.Invalid("webhook.max_connections", fmt.Sprintf("must be between 1 and %d", WebhookConnectionLimit)))
	}
	if webhook.PublicBaseURL == "" {
		if c.UsesWebhook() {
			problems = append(problems, configkit.Invalid(webhookPublicBaseURLField, "is required when an account uses webhook mode"))
		}
		return problems
	}
	if err := validatePublicWebhookURL(webhook.PublicBaseURL); err != nil {
		problems = append(problems, err)
	}
	return problems
}

func validatePublicWebhookURL(raw string) error {
	if strings.ContainsAny(raw, "?#") {
		return configkit.Invalid(webhookPublicBaseURLField, "must not carry a query or fragment")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return configkit.Invalid(webhookPublicBaseURLField, "is not a valid URL")
	}
	if u.Scheme != "https" {
		return configkit.Invalid(webhookPublicBaseURLField, "must use https")
	}
	if err := egress.ValidateBaseURL(raw); err != nil {
		return configkit.Invalid(webhookPublicBaseURLField, "is not valid: "+err.Error())
	}
	return nil
}

func userListProblems(field string, ids []string) []error {
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

func chatListProblems(field string, ids []string) []error {
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
