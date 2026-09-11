package cli

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/anggasct/aura/internal/channel/discord"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
	"github.com/anggasct/aura/internal/store"
	toolsbuiltin "github.com/anggasct/aura/internal/tools/builtin"
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

type discordSessionEnsurer struct {
	sessions store.SessionService
}

func (s *discordSessionEnsurer) EnsureSession(ctx context.Context, sessionID, ownerID string) error {
	if s.sessions == nil {
		return nil
	}
	if _, err := s.sessions.Get(ctx, sessionID); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		if code, ok := store.CodeOf(err); !ok || code != store.ErrorCodeSessionNotFound {
			return err
		}
	}
	now := time.Now().UTC()
	err := s.sessions.Create(ctx, &store.Session{
		ID:        sessionID,
		OwnerID:   ownerID,
		Metadata:  json.RawMessage(`{}`),
		CreatedAt: now,
		UpdatedAt: now,
	})
	if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeSessionIDConflict {
		return nil
	}
	return err
}

type discordMediaStore struct {
	store store.ArtifactStore
}

func (s *discordMediaStore) Put(ctx context.Context, content io.Reader, meta *discord.ArtifactMeta) (discord.ArtifactReceipt, error) {
	if s.store == nil || meta == nil {
		return discord.ArtifactReceipt{}, nil
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return discord.ArtifactReceipt{}, err
	}
	ref, err := s.store.Put(ctx, content, &store.ArtifactMetadata{
		ID:        "art-" + hex.EncodeToString(raw[:]),
		SessionID: meta.SessionID,
		Filename:  meta.Filename,
		MediaType: meta.MediaType,
		Metadata:  meta.Extra,
	})
	if err != nil {
		return discord.ArtifactReceipt{}, err
	}
	return discord.ArtifactReceipt{RefID: ref.ID, Digest: ref.BlobDigest, SizeBytes: ref.SizeBytes}, nil
}

func buildChannelAdapters(cfg *config.Config, db *sql.DB, artifactRoot string, logger *slog.Logger) ([]runtimechannelhost.ChannelPort, []health.RegisteredCheck, error) {
	if cfg == nil || !cfg.Channels.Discord.Enabled {
		return nil, nil, nil
	}
	executor, err := toolsbuiltin.NewChannelEffects(db, logger)
	if err != nil {
		return nil, nil, err
	}
	adapter, err := discord.New(&cfg.Channels.Discord, &discordResumeStore{
		store:    store.NewChannelResumeStore(db),
		instance: cfg.Channels.Discord.Instance,
	}, executor, &discordMediaStore{
		store: store.NewArtifactStore(db, artifactRoot, int64(cfg.Storage.ArtifactQuota)),
	}, &discordSessionEnsurer{sessions: store.NewSessionService(db)}, logger)
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
