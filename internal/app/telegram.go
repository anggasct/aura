package app

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/anggasct/aura/internal/channels/telegram"
	"github.com/anggasct/aura/internal/config"
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/platform/health"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
	"github.com/anggasct/aura/internal/store"
)

type telegramAdapters []*telegram.Adapter

func (adapters telegramAdapters) Check(ctx context.Context) []health.Finding {
	findings := make([]health.Finding, 0, len(adapters))
	for _, adapter := range adapters {
		findings = append(findings, adapter.Check(ctx)...)
	}
	return findings
}

func newTelegramAdapter(cfg *config.Config, account *config.TelegramAccount, db *sql.DB, artifactRoot string, logger *slog.Logger) (*telegram.Adapter, error) {
	executor, err := toolsbuiltin.NewChannelEffects(db, logger)
	if err != nil {
		return nil, err
	}
	adapter, err := telegram.New(&cfg.Channels.Telegram, account, store.NewTelegramCursorStore(db), executor, &discordSessionEnsurer{sessions: store.NewSessionService(db)}, logger)
	if err != nil {
		return nil, err
	}
	adapter.SetSessionResolver(&discordConversationResolver{pointers: store.NewConversationSessionStore(db)})
	adapter.SetSecretVault(buildSecretVault(cfg))
	adapter.SetMediaStore(&channelMediaStore{store: store.NewArtifactStore(db, artifactRoot, int64(cfg.Storage.ArtifactQuota))})
	adapter.SetUpdateLedger(store.NewDedupeStore(db))
	if svc := newVisionService(cfg, db, artifactRoot); svc != nil {
		adapter.SetVisionService(svc)
	}
	return adapter, nil
}

const telegramWebhookDrainDivisor = 2

func buildTelegramWebhookServer(cfg *config.Config, ports []runtimechannelhost.ChannelPort) (*telegram.WebhookServer, error) {
	adapters := make([]*telegram.Adapter, 0, len(ports))
	for _, port := range ports {
		if adapter, ok := port.(*telegram.Adapter); ok {
			adapters = append(adapters, adapter)
		}
	}
	handler, err := telegram.NewWebhookHandler(adapters)
	if err != nil {
		return nil, err
	}
	return telegram.NewWebhookServer(handler, cfg.Channels.Telegram.Webhook.ListenAddress, time.Duration(cfg.Runtime.ShutdownTimeout)/telegramWebhookDrainDivisor)
}
