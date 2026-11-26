package app

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/channels/kit/media"
	"github.com/anggasct/aura/internal/channels/telegram"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/storage"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
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

func newTelegramAdapter(cfg *appconfig.Config, account *config.TelegramAccount, db *sql.DB, artifactRoot string, logger *slog.Logger) (*telegram.Adapter, error) {
	executor, err := newChannelEffects(db, logger)
	if err != nil {
		return nil, err
	}
	sessions, err := access.NewSessionEnsurer(turnstore.NewSessionService(db))
	if err != nil {
		return nil, err
	}
	resolver, err := access.NewConversationResolver(turnstore.NewConversationSessionStore(db))
	if err != nil {
		return nil, err
	}
	mediaStore, err := media.NewArtifactStore(storage.NewArtifactStore(db, artifactRoot, int64(cfg.Storage.ArtifactQuota)))
	if err != nil {
		return nil, err
	}
	adapter, err := telegram.New(&cfg.Channels.Telegram, account, store.NewTelegramCursorStore(db), executor, sessions, logger)
	if err != nil {
		return nil, err
	}
	adapter.SetSessionResolver(resolver)
	adapter.SetSecretVault(buildSecretVault(cfg))
	adapter.SetMediaStore(mediaStore)
	adapter.SetUpdateLedger(turnstore.NewDedupeStore(db))
	if svc := newVisionService(cfg, db, artifactRoot); svc != nil {
		adapter.SetVisionService(svc)
	}
	return adapter, nil
}

const telegramWebhookDrainDivisor = 2

func buildTelegramWebhookServer(cfg *appconfig.Config, ports []runtimechannelhost.ChannelPort) (*telegram.WebhookServer, error) {
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
