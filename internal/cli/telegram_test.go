package cli

import (
	"strings"
	"testing"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/channel/telegram"
	"github.com/anggasct/aura/internal/config"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
)

func enabledTelegramConfig() config.Config {
	defaults := config.Default()
	defaults.Owner = &config.Owner{Identities: []string{"telegram:424242"}}
	defaults.Channels.Telegram.Enabled = true
	defaults.Channels.Telegram.Accounts = []config.TelegramAccount{{
		Name:        "pocket",
		BotTokenRef: "env://AURA_TELEGRAM_BOT_TOKEN",
		Persona:     auraagent.DefaultID,
	}}
	return defaults
}

func TestBuildChannelAdapters_TelegramBuildsAdapterAndCheck(t *testing.T) {
	db := discordTestDB(t)
	cfg := enabledTelegramConfig()
	ports, checks, approvers, err := buildChannelAdapters(&cfg, testRouter(t, &cfg), db, t.TempDir(), nil, nil)
	if err != nil {
		t.Fatalf("buildChannelAdapters(): %v", err)
	}
	if len(ports) != 1 {
		t.Fatalf("ports = %d, want 1", len(ports))
	}
	adapter, ok := ports[0].(*telegram.Adapter)
	if !ok {
		t.Fatalf("port = %T, want a telegram adapter", ports[0])
	}
	if identity, ok := ports[0].(runtimechannelhost.ChannelIdentity); !ok || identity.ChannelName() != "telegram:pocket" || adapter.Account() != "pocket" {
		t.Errorf("channel name = %v, want telegram:pocket", ports[0])
	}
	if len(checks) != 1 || checks[0].ID != "telegram" || checks[0].Remediation != telegram.RemediationReviewBot {
		t.Fatalf("checks = %+v, want the telegram check only", checks)
	}
	if len(approvers) != 1 {
		t.Error("telegram-only config built no approval channel")
	}
}

func TestBuildChannelAdapters_DiscordAndTelegramTogether(t *testing.T) {
	db := discordTestDB(t)
	cfg := enabledDiscordConfig()
	telegramCfg := enabledTelegramConfig()
	cfg.Owner = telegramCfg.Owner
	cfg.Channels.Telegram = telegramCfg.Channels.Telegram
	ports, checks, approvers, err := buildChannelAdapters(&cfg, testRouter(t, &cfg), db, t.TempDir(), nil, nil)
	if err != nil {
		t.Fatalf("buildChannelAdapters(): %v", err)
	}
	if len(ports) != 2 || len(approvers) != 2 {
		t.Fatalf("ports = %d, approvers = %d; want both channels to approve", len(ports), len(approvers))
	}
	ids := make([]string, 0, len(checks))
	for _, check := range checks {
		ids = append(ids, check.ID)
	}
	if got := strings.Join(ids, ","); got != "discord,principal,telegram" {
		t.Errorf("checks = %s", got)
	}
}

func TestBuildSecretVaultRedactsTelegramTokens(t *testing.T) {
	t.Setenv("AURA_TELEGRAM_BOT_TOKEN", "123456:telegram-secret-probe")
	cfg := enabledTelegramConfig()
	vault := buildSecretVault(&cfg)
	if got := vault.Redact("token 123456:telegram-secret-probe leaked"); strings.Contains(got, "telegram-secret-probe") {
		t.Fatalf("vault did not redact the telegram token: %q", got)
	}
}

func webhookTelegramConfig() config.Config {
	cfg := enabledTelegramConfig()
	cfg.Channels.Telegram.Accounts[0].Mode = config.TelegramModeWebhook
	cfg.Channels.Telegram.Accounts[0].WebhookSecretRef = "env://AURA_TELEGRAM_WEBHOOK_SECRET"
	cfg.Channels.Telegram.Webhook.PublicBaseURL = "https://aura.example.com"
	return cfg
}

func TestBuildTelegramWebhookServer(t *testing.T) {
	db := discordTestDB(t)
	cases := map[string]struct {
		cfg     config.Config
		wantErr bool
	}{
		"webhook account": {cfg: webhookTelegramConfig()},
		"polling only":    {cfg: enabledTelegramConfig(), wantErr: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ports, _, _, err := buildChannelAdapters(&tc.cfg, testRouter(t, &tc.cfg), db, t.TempDir(), nil, nil)
			if err != nil {
				t.Fatalf("buildChannelAdapters(): %v", err)
			}
			listener, err := buildTelegramWebhookServer(&tc.cfg, ports)
			if tc.wantErr {
				if code, ok := telegram.CodeOf(err); !ok || code != telegram.ErrorCodeInvalidArgument || listener != nil {
					t.Fatalf("buildTelegramWebhookServer() = %v, %v; want invalid_argument", listener, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildTelegramWebhookServer(): %v", err)
			}
			if listener.Name() != "telegram-webhook" {
				t.Errorf("Name() = %q", listener.Name())
			}
		})
	}
}

func TestBuildSecretVaultRedactsTelegramWebhookSecrets(t *testing.T) {
	t.Setenv("AURA_TELEGRAM_BOT_TOKEN", "123456:telegram-secret-probe")
	t.Setenv("AURA_TELEGRAM_WEBHOOK_SECRET", "hook-secret-probe-66a1")
	cfg := webhookTelegramConfig()
	vault := buildSecretVault(&cfg)
	if got := vault.Redact("header hook-secret-probe-66a1 leaked"); strings.Contains(got, "hook-secret-probe-66a1") {
		t.Fatalf("vault did not redact the webhook secret: %q", got)
	}
}
