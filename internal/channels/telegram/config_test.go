package telegram

import (
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/secret"
	"github.com/anggasct/aura/internal/platform/secret/secrettest"
)

func TestTelegramConfigValidate(t *testing.T) {
	validAccount := func(name string) Account {
		return Account{
			Name:               name,
			BotTokenRef:        secrettest.Ref("env://AURA_TELEGRAM_BOT_TOKEN"),
			Mode:               ModePolling,
			AcceptPrivateChats: true,
		}
	}
	if err := new(Config).Validate(); err == nil {
		t.Fatal("zero Config validated without error")
	}
	if err := (*Config)(nil).Validate(); !fault.HasCode(err, configkit.ErrorCodeInvalid) {
		t.Fatalf("nil Config Validate() = %v, want config_invalid", err)
	}
	defaults := Default()
	if err := defaults.Validate(); err != nil {
		t.Fatalf("Default().Validate() = %v", err)
	}

	cases := map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"enabled without accounts": {
			mutate: func(c *Config) { c.Enabled = true },
			want:   "requires at least one channels.telegram.accounts entry",
		},
		"invalid account name": {
			mutate: func(c *Config) {
				acct := validAccount("BadName")
				c.Accounts = []Account{acct}
			},
			want: "accounts[0].name",
		},
		"duplicate account name": {
			mutate: func(c *Config) {
				a := validAccount("pocket")
				b := validAccount("pocket")
				b.BotTokenRef = secrettest.Ref("env://AURA_TELEGRAM_BOT_TOKEN_2")
				c.Accounts = []Account{a, b}
			},
			want: "configured more than once",
		},
		"empty bot token ref": {
			mutate: func(c *Config) {
				acct := validAccount("pocket")
				acct.BotTokenRef = secret.Ref{}
				c.Accounts = []Account{acct}
			},
			want: "accounts[0].bot_token_ref",
		},
		"shared bot token ref": {
			mutate: func(c *Config) {
				c.Accounts = []Account{validAccount("pocket"), validAccount("spare")}
			},
			want: "is shared with another account",
		},
		"invalid allowed user id": {
			mutate: func(c *Config) {
				acct := validAccount("pocket")
				acct.AllowedUserIDs = []string{"-1"}
				c.Accounts = []Account{acct}
			},
			want: "not a positive decimal telegram user id",
		},
		"invalid allowed chat id": {
			mutate: func(c *Config) {
				acct := validAccount("pocket")
				acct.AllowedChatIDs = []string{"42"}
				c.Accounts = []Account{acct}
			},
			want: "not a negative decimal telegram group chat id",
		},
		"webhook secret in polling mode": {
			mutate: func(c *Config) {
				acct := validAccount("pocket")
				acct.WebhookSecretRef = secrettest.Ref("env://AURA_WEBHOOK_SECRET")
				c.Accounts = []Account{acct}
			},
			want: "is only valid when mode is webhook",
		},
		"webhook mode missing secret": {
			mutate: func(c *Config) {
				acct := validAccount("pocket")
				acct.Mode = ModeWebhook
				c.Accounts = []Account{acct}
				c.Webhook.PublicBaseURL = "https://aura.example.com"
			},
			want: "must not be empty in webhook mode",
		},
		"webhook secret equals bot token ref": {
			mutate: func(c *Config) {
				acct := validAccount("pocket")
				acct.Mode = ModeWebhook
				acct.WebhookSecretRef = acct.BotTokenRef
				c.Accounts = []Account{acct}
				c.Webhook.PublicBaseURL = "https://aura.example.com"
			},
			want: "must differ from every bot token reference",
		},
		"unknown mode": {
			mutate: func(c *Config) {
				acct := validAccount("pocket")
				acct.Mode = "pigeon"
				c.Accounts = []Account{acct}
			},
			want: "must be polling or webhook",
		},
		"webhook missing public base url": {
			mutate: func(c *Config) {
				acct := validAccount("pocket")
				acct.Mode = ModeWebhook
				acct.WebhookSecretRef = secrettest.Ref("env://AURA_WEBHOOK_SECRET")
				c.Accounts = []Account{acct}
			},
			want: "is required when an account uses webhook mode",
		},
		"webhook non-https public base url": {
			mutate: func(c *Config) {
				c.Webhook.PublicBaseURL = "http://aura.example.com"
			},
			want: "must use https",
		},
		"webhook query in public base url": {
			mutate: func(c *Config) {
				c.Webhook.PublicBaseURL = "https://aura.example.com?a=1"
			},
			want: "must not carry a query or fragment",
		},
		"webhook bad listen address": {
			mutate: func(c *Config) { c.Webhook.ListenAddress = "not-an-addr" },
			want:   "webhook.listen_address",
		},
		"webhook max body bytes too large": {
			mutate: func(c *Config) { c.Webhook.MaxBodyBytes = WebhookMaxBodyCeiling + 1 },
			want:   "webhook.max_body_bytes",
		},
		"webhook max connections too large": {
			mutate: func(c *Config) { c.Webhook.MaxConnections = WebhookConnectionLimit + 1 },
			want:   "webhook.max_connections",
		},
		"fractional polling timeout": {
			mutate: func(c *Config) { c.Polling.Timeout = configkit.Duration(1500 * time.Millisecond) },
			want:   "polling.timeout",
		},
		"polling backoff out of range": {
			mutate: func(c *Config) { c.Polling.MaxBackoff = configkit.Duration(500 * time.Millisecond) },
			want:   "polling.max_backoff",
		},
		"min edit interval out of range": {
			mutate: func(c *Config) { c.MinEditInterval = configkit.Duration(500 * time.Millisecond) },
			want:   "min_edit_interval",
		},
		"max attachment bytes out of range": {
			mutate: func(c *Config) { c.MaxAttachmentBytes = MaxAttachmentBytes + 1 },
			want:   "max_attachment_bytes",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() succeeded, want error containing %q", tc.want)
			}
			if !fault.HasCode(err, configkit.ErrorCodeInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want config_invalid containing %q", err, tc.want)
			}
		})
	}
}
