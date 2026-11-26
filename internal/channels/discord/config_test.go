package discord

import (
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/secret"
	"github.com/anggasct/aura/internal/platform/secret/secrettest"
)

func TestDiscordConfigValidate(t *testing.T) {
	baseAccount := func() Account {
		return Account{
			Name:           "home",
			BotTokenRef:    secrettest.Ref("env://DISCORD_HOME_TOKEN"),
			AllowedUserIDs: []string{"123456789012345678"},
			AcceptDMs:      true,
		}
	}

	t.Run("default config is valid", func(t *testing.T) {
		cfg := Default()
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Default().Validate() = %v, want nil", err)
		}
	})

	t.Run("enabled valid config passes", func(t *testing.T) {
		cfg := Default()
		cfg.Enabled = true
		cfg.Accounts = []Account{baseAccount()}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
	})

	t.Run("nil receiver is rejected", func(t *testing.T) {
		var cfg *Config
		err := cfg.Validate()
		if !fault.HasCode(err, configkit.ErrorCodeInvalid) {
			t.Fatalf("Validate() = %v, want config_invalid", err)
		}
	})

	invalid := map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"enabled without accounts": {
			mutate: func(c *Config) { c.Enabled = true },
			want:   "requires at least one channels.discord.accounts entry",
		},
		"enabled account without allowed_user_ids": {
			mutate: func(c *Config) {
				c.Enabled = true
				acct := baseAccount()
				acct.AllowedUserIDs = nil
				c.Accounts = []Account{acct}
			},
			want: "requires at least one allowed_user_ids entry when channels.discord is enabled",
		},
		"bad account name": {
			mutate: func(c *Config) {
				acct := baseAccount()
				acct.Name = "home/bad"
				c.Accounts = []Account{acct}
			},
			want: "accounts[0].name",
		},
		"duplicate account name": {
			mutate: func(c *Config) {
				a := baseAccount()
				b := baseAccount()
				b.BotTokenRef = secrettest.Ref("env://DISCORD_OTHER_TOKEN")
				c.Accounts = []Account{a, b}
			},
			want: "is configured more than once",
		},
		"missing bot token ref": {
			mutate: func(c *Config) {
				acct := baseAccount()
				acct.BotTokenRef = secret.Ref{}
				c.Accounts = []Account{acct}
			},
			want: "accounts[0].bot_token_ref",
		},
		"shared bot token ref": {
			mutate: func(c *Config) {
				a := baseAccount()
				b := baseAccount()
				b.Name = "kuma"
				c.Accounts = []Account{a, b}
			},
			want: "is shared with another account",
		},
		"empty snowflake id": {
			mutate: func(c *Config) {
				acct := baseAccount()
				acct.AllowedUserIDs = []string{""}
				c.Accounts = []Account{acct}
			},
			want: "must not contain empty ids",
		},
		"non-numeric snowflake": {
			mutate: func(c *Config) {
				acct := baseAccount()
				acct.AllowedGuildIDs = []string{"not-a-snowflake"}
				c.Accounts = []Account{acct}
			},
			want: "is not a decimal snowflake",
		},
		"duplicate snowflake": {
			mutate: func(c *Config) {
				acct := baseAccount()
				acct.AllowedChannelIDs = []string{"123456789012345678", "123456789012345678"}
				c.Accounts = []Account{acct}
			},
			want: "is configured more than once",
		},
		"non-positive min_edit_interval": {
			mutate: func(c *Config) { c.MinEditInterval = 0 },
			want:   "min_edit_interval",
		},
		"non-positive max_attachment_bytes": {
			mutate: func(c *Config) { c.MaxAttachmentBytes = 0 },
			want:   "max_attachment_bytes",
		},
		"non-positive thread_auto_archive_minutes": {
			mutate: func(c *Config) { c.ThreadAutoArchiveMinutes = 0 },
			want:   "thread_auto_archive_minutes",
		},
	}

	for name, tc := range invalid {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if !fault.HasCode(err, configkit.ErrorCodeInvalid) {
				t.Fatalf("Validate() = %v, want config_invalid", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate() = %v, want substring %q", err, tc.want)
			}
		})
	}
}
