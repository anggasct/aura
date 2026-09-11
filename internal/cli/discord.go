package cli

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/anggasct/aura/internal/channel/discord"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
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

func buildChannelAdapters(cfg *config.Config, db *sql.DB, logger *slog.Logger) ([]runtimechannelhost.ChannelPort, []health.RegisteredCheck, error) {
	if cfg == nil || !cfg.Channels.Discord.Enabled {
		return nil, nil, nil
	}
	adapter, err := discord.New(&cfg.Channels.Discord, &discordResumeStore{
		store:    store.NewChannelResumeStore(db),
		instance: cfg.Channels.Discord.Instance,
	}, logger)
	if err != nil {
		return nil, nil, err
	}
	checks := []health.RegisteredCheck{{
		ID:          "discord",
		Checker:     adapter,
		Timeout:     time.Duration(cfg.Health.CheckTimeout),
		Remediation: discord.RemediationReviewGateway,
	}}
	return []runtimechannelhost.ChannelPort{adapter}, checks, nil
}
