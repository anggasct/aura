package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/anggasct/aura/internal/channels/discord"
	"github.com/anggasct/aura/internal/channels/email"
	"github.com/anggasct/aura/internal/channels/telegram"
	"github.com/anggasct/aura/internal/config"
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/secret"
	"github.com/anggasct/aura/internal/platform/telemetry"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
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

type discordSessionEnsurer struct {
	sessions store.SessionService
}

func (s *discordSessionEnsurer) SessionExists(ctx context.Context, sessionID string) (bool, error) {
	if s.sessions == nil {
		return false, nil
	}
	_, err := s.sessions.Get(ctx, sessionID)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeSessionNotFound {
		return false, nil
	}
	return false, err
}

func (s *discordSessionEnsurer) EnsureSession(ctx context.Context, sessionID, ownerID, agentID string) error {
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
		AgentID:   agentID,
		Metadata:  json.RawMessage(`{}`),
		CreatedAt: now,
		UpdatedAt: now,
	})
	if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeSessionIDConflict {
		return nil
	}
	return err
}

type discordAdapters []*discord.Adapter

func (adapters discordAdapters) Check(ctx context.Context) []health.Finding {
	findings := make([]health.Finding, 0, len(adapters))
	for _, adapter := range adapters {
		findings = append(findings, adapter.Check(ctx)...)
	}
	return findings
}

func buildChannelAdapters(cfg *config.Config, router *auraagent.Router, db *sql.DB, artifactRoot string, mp metric.MeterProvider, logger *slog.Logger) ([]runtimechannelhost.ChannelPort, []health.RegisteredCheck, []approvalAdapter, error) {
	if cfg == nil || (!cfg.Channels.Discord.Enabled && !cfg.Channels.Telegram.Enabled && !cfg.Channels.Email.Enabled) {
		return nil, nil, nil, nil
	}
	if router == nil {
		return nil, nil, nil, errors.New("persona router must not be nil")
	}
	owners, err := ownerResolver(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	recorder, err := telemetry.NewPrincipalRecorder(mp)
	if err != nil {
		return nil, nil, nil, err
	}
	accessRecorder, err := telemetry.NewAccessRecorder(mp)
	if err != nil {
		return nil, nil, nil, err
	}
	ingressRecorder, err := telemetry.NewIngressRecorder(mp)
	if err != nil {
		return nil, nil, nil, err
	}
	var ports []runtimechannelhost.ChannelPort
	var checks []health.RegisteredCheck
	var approvers []approvalAdapter
	if cfg.Channels.Discord.Enabled {
		adapters := make(discordAdapters, 0, len(cfg.Channels.Discord.Accounts))
		for i := range cfg.Channels.Discord.Accounts {
			adapter, err := newDiscordAdapter(cfg, &cfg.Channels.Discord.Accounts[i], db, artifactRoot, logger)
			if err != nil {
				return nil, nil, nil, err
			}
			adapter.SetPrincipalResolver(owners)
			adapter.SetPersonaRouter(router)
			settings := discordAccessSettings(cfg, db, accessRecorder)
			adapter.SetAccess(&settings)
			adapter.SetAdmissionObserver(func(ctx context.Context, class principal.Class) {
				recorder.Admitted(ctx, string(class), principal.ChannelDiscord)
			})
			adapter.SetDropObserver(func(ctx context.Context, reason string) {
				ingressRecorder.Dropped(ctx, principal.ChannelDiscord, reason)
			})
			adapters = append(adapters, adapter)
			ports = append(ports, adapter)
			approvers = append(approvers, adapter)
		}
		checks = append(checks, health.RegisteredCheck{
			ID:          "discord",
			Checker:     adapters,
			Timeout:     time.Duration(cfg.Health.CheckTimeout),
			Remediation: discord.RemediationReviewGateway,
		}, health.RegisteredCheck{
			ID:          "principal",
			Checker:     identityMappingCheck{owners: owners},
			Timeout:     time.Duration(cfg.Health.CheckTimeout),
			Remediation: remediationMapOwnerIdentity,
		})
	}
	if cfg.Channels.Telegram.Enabled {
		adapters := make(telegramAdapters, 0, len(cfg.Channels.Telegram.Accounts))
		for i := range cfg.Channels.Telegram.Accounts {
			adapter, err := newTelegramAdapter(cfg, &cfg.Channels.Telegram.Accounts[i], db, artifactRoot, logger)
			if err != nil {
				return nil, nil, nil, err
			}
			adapter.SetPrincipalResolver(owners)
			adapter.SetPersonaRouter(router)
			settings := telegramAccessSettings(cfg, db, accessRecorder)
			adapter.SetAccess(&settings)
			adapter.SetAdmissionObserver(func(ctx context.Context, class principal.Class) {
				recorder.Admitted(ctx, string(class), principal.ChannelTelegram)
			})
			adapter.SetDenialObserver(accessRecorder.Denied)
			adapter.SetDropObserver(func(ctx context.Context, reason string) {
				ingressRecorder.Dropped(ctx, principal.ChannelTelegram, reason)
			})
			adapters = append(adapters, adapter)
			ports = append(ports, adapter)
			approvers = append(approvers, adapter)
		}
		checks = append(checks, health.RegisteredCheck{
			ID:          "telegram",
			Checker:     adapters,
			Timeout:     time.Duration(cfg.Health.CheckTimeout),
			Remediation: telegram.RemediationReviewBot,
		})
	}
	if cfg.Channels.Email.Enabled {
		adapters := make(emailAdapters, 0, len(cfg.Channels.Email.Accounts))
		for i := range cfg.Channels.Email.Accounts {
			adapter, err := newEmailAdapter(cfg, &cfg.Channels.Email.Accounts[i], db, artifactRoot, logger)
			if err != nil {
				return nil, nil, nil, err
			}
			adapter.SetPrincipalResolver(owners)
			adapter.SetPersonaRouter(router)
			settings := discordAccessSettings(cfg, db, accessRecorder)
			adapter.SetAccess(&email.AccessSettings{Budgets: settings.Budgets, Evals: settings.Evals, Handoffs: settings.Handoffs, Erasure: settings.Erasure})
			adapter.SetAdmissionObserver(func(ctx context.Context, class principal.Class) {
				recorder.Admitted(ctx, string(class), principal.ChannelEmail)
			})
			adapter.SetDropObserver(func(ctx context.Context, reason string) {
				ingressRecorder.Dropped(ctx, principal.ChannelEmail, reason)
			})
			adapters = append(adapters, adapter)
			ports = append(ports, adapter)
		}
		checks = append(checks, health.RegisteredCheck{
			ID:          "email",
			Checker:     adapters,
			Timeout:     time.Duration(cfg.Health.CheckTimeout),
			Remediation: email.RemediationReviewMailbox,
		})
	}
	return ports, checks, approvers, nil
}

func newDiscordAdapter(cfg *config.Config, account *config.DiscordAccount, db *sql.DB, artifactRoot string, logger *slog.Logger) (*discord.Adapter, error) {
	executor, err := toolsbuiltin.NewChannelEffects(db, logger)
	if err != nil {
		return nil, err
	}
	adapter, err := discord.New(&cfg.Channels.Discord, account, &discordResumeStore{
		store:    store.NewChannelResumeStore(db),
		instance: account.Name,
	}, executor, &channelMediaStore{
		store: store.NewArtifactStore(db, artifactRoot, int64(cfg.Storage.ArtifactQuota)),
	}, &discordSessionEnsurer{sessions: store.NewSessionService(db)}, logger)
	if err != nil {
		return nil, err
	}
	adapter.SetSessionResolver(&discordConversationResolver{pointers: store.NewConversationSessionStore(db)})
	adapter.SetSecretVault(buildSecretVault(cfg))
	if svc := newVisionService(cfg, db, artifactRoot); svc != nil {
		adapter.SetVisionService(svc)
	}
	return adapter, nil
}

func buildSecretVault(cfg *config.Config) *secret.Vault {
	vault := secret.New()
	for _, entry := range configuredSecrets(cfg) {
		if value, err := entry.resolve(); err == nil && strings.TrimSpace(value) != "" {
			vault.Set(entry.name, value)
		}
	}
	return vault
}

func parseSecretRef(raw string) secret.Reference {
	switch {
	case strings.HasPrefix(raw, "env://"):
		return secret.Reference{Env: strings.TrimPrefix(raw, "env://")}
	case strings.HasPrefix(raw, "file://"):
		return secret.Reference{File: strings.TrimPrefix(raw, "file://")}
	default:
		return secret.Reference{}
	}
}
