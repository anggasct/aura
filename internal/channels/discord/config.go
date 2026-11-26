package discord

import (
	"errors"
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/secret"
)

type Config struct {
	Enabled                  bool               `json:"enabled"`
	Accounts                 []Account          `json:"accounts"`
	MinEditInterval          configkit.Duration `json:"min_edit_interval"`
	MaxAttachmentBytes       int64              `json:"max_attachment_bytes"`
	ThreadAutoArchiveMinutes int                `json:"thread_auto_archive_minutes"`
}

type Account struct {
	Name              string     `json:"name"`
	BotTokenRef       secret.Ref `json:"bot_token_ref"`
	Persona           string     `json:"persona,omitempty"`
	AllowedUserIDs    []string   `json:"allowed_user_ids"`
	AllowedGuildIDs   []string   `json:"allowed_guild_ids"`
	AllowedChannelIDs []string   `json:"allowed_channel_ids"`
	AcceptDMs         bool       `json:"accept_dms"`
	LinkPreviews      bool       `json:"link_previews"`
}

func (*Account) Default() any {
	return Account{AcceptDMs: true}
}

func Default() Config {
	return Config{
		MinEditInterval:          configkit.Duration(2 * time.Second),
		MaxAttachmentBytes:       20971520,
		ThreadAutoArchiveMinutes: 1440,
	}
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
		problems = append(problems, snowflakeListProblems(field+".allowed_user_ids", account.AllowedUserIDs)...)
		problems = append(problems, snowflakeListProblems(field+".allowed_guild_ids", account.AllowedGuildIDs)...)
		problems = append(problems, snowflakeListProblems(field+".allowed_channel_ids", account.AllowedChannelIDs)...)
		if c.Enabled && len(account.AllowedUserIDs) == 0 {
			problems = append(problems, configkit.Invalid(field, "requires at least one allowed_user_ids entry when channels.discord is enabled"))
		}
	}
	if c.Enabled && len(c.Accounts) == 0 {
		problems = append(problems, configkit.Invalid("enabled", "requires at least one channels.discord.accounts entry"))
	}
	if c.MinEditInterval <= 0 {
		problems = append(problems, configkit.Invalid("min_edit_interval", "must be positive"))
	}
	if c.MaxAttachmentBytes <= 0 {
		problems = append(problems, configkit.Invalid("max_attachment_bytes", "must be positive"))
	}
	if c.ThreadAutoArchiveMinutes <= 0 {
		problems = append(problems, configkit.Invalid("thread_auto_archive_minutes", "must be positive"))
	}
	return errors.Join(problems...)
}

func snowflakeListProblems(field string, ids []string) []error {
	var problems []error
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		switch {
		case id == "":
			problems = append(problems, configkit.Invalid(field, "must not contain empty ids"))
		case !principal.IsDecimalSnowflake(id):
			problems = append(problems, configkit.Invalid(field, fmt.Sprintf("id %q is not a decimal snowflake", id)))
		case seen[id]:
			problems = append(problems, configkit.Invalid(field, fmt.Sprintf("id %q is configured more than once", id)))
		default:
			seen[id] = true
		}
	}
	return problems
}
