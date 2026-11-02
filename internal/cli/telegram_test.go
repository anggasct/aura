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
	ports, checks, decider, err := buildChannelAdapters(&cfg, testRouter(t, &cfg), db, t.TempDir(), nil, nil)
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
	if decider != nil {
		t.Error("telegram-only config built an approval decider before approval cards exist")
	}
}

func TestBuildChannelAdapters_DiscordAndTelegramTogether(t *testing.T) {
	db := discordTestDB(t)
	cfg := enabledDiscordConfig()
	telegramCfg := enabledTelegramConfig()
	cfg.Owner = telegramCfg.Owner
	cfg.Channels.Telegram = telegramCfg.Channels.Telegram
	ports, checks, decider, err := buildChannelAdapters(&cfg, testRouter(t, &cfg), db, t.TempDir(), nil, nil)
	if err != nil {
		t.Fatalf("buildChannelAdapters(): %v", err)
	}
	if len(ports) != 2 || decider == nil {
		t.Fatalf("ports = %d, decider nil = %v; want both channels and the discord decider", len(ports), decider == nil)
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
