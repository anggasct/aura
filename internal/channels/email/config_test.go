package email

import (
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/secret"
	"github.com/anggasct/aura/internal/platform/secret/secrettest"
)

func validEmailConfig() Config {
	return Config{
		Enabled:            true,
		MaxAttachmentBytes: MaxAttachmentBytes,
		Accounts: []Account{{
			Name:       "work",
			Persona:    "main",
			Address:    "aura@example.com",
			AuthservID: "mx.example.com",
			IMAP: IMAPConfig{
				Host:         "imap.example.com",
				Port:         DefaultIMAPPort,
				Username:     "aura@example.com",
				PasswordRef:  secrettest.Ref("env://AURA_EMAIL_PASSWORD"),
				Folder:       DefaultFolder,
				PollInterval: configkit.Duration(DefaultPollInterval),
			},
			SMTP: SMTPConfig{
				Host:        "smtp.example.com",
				Port:        DefaultSMTPPort,
				Username:    "aura@example.com",
				PasswordRef: secrettest.Ref("env://AURA_EMAIL_SMTP_PASSWORD"),
			},
			AllowedSenders: []string{"friend@example.com"},
		}},
	}
}

func TestEmailConfigValidate(t *testing.T) {
	t.Run("nil config", func(t *testing.T) {
		var cfg *Config
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "must not be nil") {
			t.Fatalf("Validate() = %v, want nil-config rejection", err)
		}
	})

	t.Run("default disabled config is valid", func(t *testing.T) {
		cfg := Default()
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Default().Validate() = %v, want nil", err)
		}
	})

	t.Run("account defaults", func(t *testing.T) {
		var acc Account
		got, ok := acc.Default().(Account)
		if !ok {
			t.Fatalf("Default() type = %T, want Account", acc.Default())
		}
		if got.IMAP.Port != DefaultIMAPPort || got.IMAP.Folder != DefaultFolder || got.IMAP.PollInterval != configkit.Duration(DefaultPollInterval) || got.SMTP.Port != DefaultSMTPPort {
			t.Fatalf("Account.Default() = %+v", got)
		}
	})

	cases := map[string]struct {
		mutate  func(*Config)
		wantErr string
	}{
		"valid enabled config": {},
		"enabled without accounts": {
			mutate:  func(c *Config) { c.Accounts = nil },
			wantErr: "enabled: config_invalid: requires at least one channels.email.accounts entry",
		},
		"zero max attachment bytes": {
			mutate:  func(c *Config) { c.MaxAttachmentBytes = 0 },
			wantErr: "max_attachment_bytes: config_invalid: must be between 1 and 20971520",
		},
		"max attachment bytes above ceiling": {
			mutate:  func(c *Config) { c.MaxAttachmentBytes = MaxAttachmentBytes + 1 },
			wantErr: "max_attachment_bytes: config_invalid: must be between 1 and 20971520",
		},
		"invalid account name": {
			mutate:  func(c *Config) { c.Accounts[0].Name = "Work/1" },
			wantErr: "accounts[0].name: config_invalid:",
		},
		"duplicate account name": {
			mutate:  func(c *Config) { c.Accounts = append(c.Accounts, c.Accounts[0]) },
			wantErr: `accounts[1].name: config_invalid: "work" is configured more than once`,
		},
		"missing persona": {
			mutate:  func(c *Config) { c.Accounts[0].Persona = "" },
			wantErr: "accounts[0].persona: config_invalid: must not be empty",
		},
		"uppercase mailbox address": {
			mutate:  func(c *Config) { c.Accounts[0].Address = "Aura@example.com" },
			wantErr: "accounts[0].address: config_invalid: must be a lowercase mailbox address",
		},
		"non-plain mailbox address": {
			mutate:  func(c *Config) { c.Accounts[0].Address = "Aura <aura@example.com>" },
			wantErr: "accounts[0].address: config_invalid: must be a lowercase mailbox address",
		},
		"invalid authserv_id": {
			mutate:  func(c *Config) { c.Accounts[0].AuthservID = "mx.example.com (bad)" },
			wantErr: "accounts[0].authserv_id: config_invalid: must be the receiving server's host name",
		},
		"invalid imap host": {
			mutate:  func(c *Config) { c.Accounts[0].IMAP.Host = "imaps://imap.example.com" },
			wantErr: "accounts[0].imap.host: config_invalid: must be a host name",
		},
		"imap port out of range": {
			mutate:  func(c *Config) { c.Accounts[0].IMAP.Port = 0 },
			wantErr: "accounts[0].imap.port: config_invalid: must be between 1 and 65535",
		},
		"imap empty username": {
			mutate:  func(c *Config) { c.Accounts[0].IMAP.Username = "" },
			wantErr: "accounts[0].imap.username: config_invalid: must be a single line",
		},
		"imap missing password_ref": {
			mutate:  func(c *Config) { c.Accounts[0].IMAP.PasswordRef = secret.Ref{} },
			wantErr: "accounts[0].imap.password_ref: config_invalid: must not be empty",
		},
		"imap control char folder": {
			mutate:  func(c *Config) { c.Accounts[0].IMAP.Folder = "INBOX\nA1" },
			wantErr: "accounts[0].imap.folder: config_invalid: must be a single line",
		},
		"imap poll interval below min": {
			mutate:  func(c *Config) { c.Accounts[0].IMAP.PollInterval = configkit.Duration(5 * time.Second) },
			wantErr: "accounts[0].imap.poll_interval: config_invalid: must be between 10s and 1h0m0s",
		},
		"imap poll interval above max": {
			mutate:  func(c *Config) { c.Accounts[0].IMAP.PollInterval = configkit.Duration(2 * time.Hour) },
			wantErr: "accounts[0].imap.poll_interval: config_invalid: must be between 10s and 1h0m0s",
		},
		"disabled without smtp is valid": {
			mutate: func(c *Config) {
				c.Enabled = false
				c.Accounts[0].SMTP = SMTPConfig{Port: DefaultSMTPPort}
			},
		},
		"disabled with partial smtp fails": {
			mutate: func(c *Config) {
				c.Enabled = false
				c.Accounts[0].SMTP = SMTPConfig{Host: "smtp.example.com", Port: DefaultSMTPPort}
			},
			wantErr: "accounts[0].smtp.password_ref: config_invalid: must not be empty",
		},
		"enabled without smtp fails": {
			mutate:  func(c *Config) { c.Accounts[0].SMTP = SMTPConfig{Port: DefaultSMTPPort} },
			wantErr: "accounts[0].smtp.host: config_invalid: must be a host name",
		},
		"uppercase allowed sender": {
			mutate:  func(c *Config) { c.Accounts[0].AllowedSenders = []string{"Friend@example.com"} },
			wantErr: "accounts[0].allowed_senders: config_invalid: entries must be lowercase mailbox addresses",
		},
		"duplicate allowed sender": {
			mutate:  func(c *Config) { c.Accounts[0].AllowedSenders = []string{"friend@example.com", "friend@example.com"} },
			wantErr: "accounts[0].allowed_senders: config_invalid: lists an address more than once",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validEmailConfig()
			if tc.mutate != nil {
				tc.mutate(&cfg)
			}
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}
