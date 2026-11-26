package app

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/channels/discord"
	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/channels/kit/media"
	"github.com/anggasct/aura/internal/config"
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/secret"
	"github.com/anggasct/aura/internal/platform/storage"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/store"
)

type discordResumeStore struct {
	store    store.ChannelResumeStore
	instance string
}

func (s *discordResumeStore) Save(ctx context.Context, cursor *discord.ResumeCursor) error {
	if s.store == nil || cursor == nil {
		return nil
	}
	return s.store.Save(ctx, &store.ChannelResume{
		Source:           "discord",
		Instance:         s.instance,
		GatewaySessionID: cursor.SessionID,
		LastSequence:     cursor.Sequence,
		ConfigDigest:     cursor.ConfigDigest,
		UpdatedAt:        cursor.UpdatedAt,
	})
}

func (s *discordResumeStore) Load(ctx context.Context) (discord.ResumeCursor, bool, error) {
	if s.store == nil {
		return discord.ResumeCursor{}, false, nil
	}
	resume, found, err := s.store.Load(ctx, "discord", s.instance)
	if err != nil || !found {
		return discord.ResumeCursor{}, false, err
	}
	return discord.ResumeCursor{
		SessionID:    resume.GatewaySessionID,
		Sequence:     resume.LastSequence,
		ConfigDigest: resume.ConfigDigest,
		UpdatedAt:    resume.UpdatedAt,
	}, true, nil
}

func (s *discordResumeStore) Delete(ctx context.Context) error {
	if s.store == nil {
		return nil
	}
	return s.store.Delete(ctx, "discord", s.instance)
}

type discordAdapters []*discord.Adapter

func (adapters discordAdapters) Check(ctx context.Context) []health.Finding {
	findings := make([]health.Finding, 0, len(adapters))
	for _, adapter := range adapters {
		findings = append(findings, adapter.Check(ctx)...)
	}
	return findings
}

func newDiscordAdapter(cfg *appconfig.Config, account *config.DiscordAccount, db *sql.DB, artifactRoot string, logger *slog.Logger) (*discord.Adapter, error) {
	executor, err := toolsbuiltin.NewChannelEffects(db, logger)
	if err != nil {
		return nil, err
	}
	mediaStore, err := media.NewArtifactStore(storage.NewArtifactStore(db, artifactRoot, int64(cfg.Storage.ArtifactQuota)))
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
	adapter, err := discord.New(&cfg.Channels.Discord, account, &discordResumeStore{
		store:    store.NewChannelResumeStore(db),
		instance: account.Name,
	}, executor, mediaStore, sessions, logger)
	if err != nil {
		return nil, err
	}
	adapter.SetSessionResolver(resolver)
	adapter.SetSecretVault(buildSecretVault(cfg))
	if svc := newVisionService(cfg, db, artifactRoot); svc != nil {
		adapter.SetVisionService(svc)
	}
	return adapter, nil
}

func buildSecretVault(cfg *appconfig.Config) *secret.Vault {
	vault := secret.New()
	for _, entry := range configuredSecrets(cfg) {
		if value, err := entry.resolve(); err == nil && strings.TrimSpace(value) != "" {
			vault.Set(entry.name, value)
		}
	}
	return vault
}
