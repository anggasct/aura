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
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/metric"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/channel/discord"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/principal"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
	"github.com/anggasct/aura/internal/secret"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/telemetry"
	"github.com/anggasct/aura/internal/toolbroker"
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

type discordAdapters []*discord.Adapter

func (adapters discordAdapters) Check(ctx context.Context) []health.Finding {
	findings := make([]health.Finding, 0, len(adapters))
	for _, adapter := range adapters {
		findings = append(findings, adapter.Check(ctx)...)
	}
	return findings
}

func buildChannelAdapters(cfg *config.Config, router *auraagent.Router, db *sql.DB, artifactRoot string, mp metric.MeterProvider, logger *slog.Logger) ([]runtimechannelhost.ChannelPort, []health.RegisteredCheck, toolbroker.ApprovalDecider, error) {
	if cfg == nil || !cfg.Channels.Discord.Enabled {
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
	adapters := make(discordAdapters, 0, len(cfg.Channels.Discord.Accounts))
	ports := make([]runtimechannelhost.ChannelPort, 0, len(cfg.Channels.Discord.Accounts))
	for i := range cfg.Channels.Discord.Accounts {
		adapter, err := newDiscordAdapter(cfg, &cfg.Channels.Discord.Accounts[i], db, artifactRoot, logger)
		if err != nil {
			return nil, nil, nil, err
		}
		adapter.SetPrincipalResolver(owners)
		adapter.SetPersonaRouter(router)
		adapter.SetAdmissionObserver(func(ctx context.Context, class principal.Class) {
			recorder.Admitted(ctx, string(class), principal.ChannelDiscord)
		})
		adapters = append(adapters, adapter)
		ports = append(ports, adapter)
	}
	checks := []health.RegisteredCheck{{
		ID:          "discord",
		Checker:     adapters,
		Timeout:     time.Duration(cfg.Health.CheckTimeout),
		Remediation: discord.RemediationReviewGateway,
	}, {
		ID:          "principal",
		Checker:     identityMappingCheck{owners: owners},
		Timeout:     time.Duration(cfg.Health.CheckTimeout),
		Remediation: remediationMapOwnerIdentity,
	}}
	return ports, checks, discordApprovalDecider(adapters), nil
}

func newDiscordAdapter(cfg *config.Config, account *config.DiscordAccount, db *sql.DB, artifactRoot string, logger *slog.Logger) (*discord.Adapter, error) {
	executor, err := toolsbuiltin.NewChannelEffects(db, logger)
	if err != nil {
		return nil, err
	}
	adapter, err := discord.New(&cfg.Channels.Discord, account, &discordResumeStore{
		store:    store.NewChannelResumeStore(db),
		instance: account.Name,
	}, executor, &discordMediaStore{
		store: store.NewArtifactStore(db, artifactRoot, int64(cfg.Storage.ArtifactQuota)),
	}, &discordSessionEnsurer{sessions: store.NewSessionService(db)}, logger)
	if err != nil {
		return nil, err
	}
	adapter.SetSecretVault(buildSecretVault(cfg))
	if svc := newVisionService(cfg, db, artifactRoot); svc != nil {
		adapter.SetVisionService(svc)
	}
	return adapter, nil
}

func buildSecretVault(cfg *config.Config) *secret.Vault {
	vault := secret.New()
	for i := range cfg.Channels.Discord.Accounts {
		account := &cfg.Channels.Discord.Accounts[i]
		if ref := parseSecretRef(account.BotTokenRef); ref.Env != "" {
			if val, err := ref.Resolve(); err == nil && strings.TrimSpace(val) != "" {
				vault.Set("discord_bot_token_"+account.Name, val)
			}
		}
	}
	if cfg.Sync != nil {
		if ref := parseSecretRef(cfg.Sync.TransportSecretRef); ref.Env != "" {
			if val, err := ref.Resolve(); err == nil && strings.TrimSpace(val) != "" {
				vault.Set("sync_transport_secret", val)
			}
		}
	}
	for _, key := range cfg.Webhook.Keys {
		if key.SecretEnv != "" {
			if val, ok := os.LookupEnv(key.SecretEnv); ok && strings.TrimSpace(val) != "" {
				vault.Set("webhook_"+key.ID, val)
			}
		}
	}
	for name := range cfg.Models.Definitions {
		def := cfg.Models.Definitions[name]
		if def.APIKeyEnv != "" {
			if val, ok := os.LookupEnv(def.APIKeyEnv); ok && strings.TrimSpace(val) != "" {
				vault.Set("model_"+def.Provider, val)
			}
		}
		if def.APIKeyFile != "" {
			if data, err := os.ReadFile(def.APIKeyFile); err == nil {
				if val := strings.TrimSpace(string(data)); val != "" {
					vault.Set("model_"+def.Provider, val)
				}
			}
		}
	}
	for name, preset := range cfg.Models.Providers {
		for hname, hval := range preset.Headers {
			if strings.TrimSpace(hval) == "" {
				continue
			}
			if strings.Contains(strings.ToLower(hname), "authorization") ||
				strings.Contains(strings.ToLower(hname), "api") ||
				strings.Contains(strings.ToLower(hname), "token") ||
				strings.Contains(strings.ToLower(hname), "secret") {
				vault.Set("provider_header_"+name+"_"+hname, hval)
			}
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

func discordApprovalDecider(adapters discordAdapters) toolbroker.ApprovalDecider {
	if len(adapters) == 0 {
		return nil
	}
	return func(ctx context.Context, prompt *toolbroker.ApprovalPrompt) (bool, error) {
		if prompt == nil {
			return false, errors.New("approval prompt must not be nil")
		}
		adapter := adapters[0]
		for _, candidate := range adapters {
			if candidate.HoldsApprovalTarget(prompt.SessionID) {
				adapter = candidate
				break
			}
		}
		return adapter.Decide(ctx, &discord.ApprovalPrompt{
			ToolName:       prompt.ToolName,
			ToolVersion:    prompt.ToolVersion,
			SessionID:      prompt.SessionID,
			TurnID:         prompt.TurnID,
			PrincipalID:    prompt.PrincipalID,
			Arguments:      prompt.Arguments,
			Network:        prompt.Network,
			Timeout:        prompt.Timeout,
			MaxOutputBytes: prompt.MaxOutputBytes,
			PolicyVersion:  prompt.PolicyVersion,
			ReasonCode:     prompt.ReasonCode,
			ExpiresAt:      prompt.ExpiresAt,
		})
	}
}
