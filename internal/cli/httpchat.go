package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/channel/httpchat"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/runtime"
	runtimeengine "github.com/anggasct/aura/internal/runtime/engine"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/telemetry"
)

const httpChatDrainDivisor = 2

type apiKeyDirectory struct {
	keys store.APIKeyStore
}

func (d *apiKeyDirectory) GetByID(ctx context.Context, id string) (httpchat.Key, bool, error) {
	key, err := d.keys.GetByID(ctx, id)
	if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeAPIKeyNotFound {
		return httpchat.Key{}, false, nil
	}
	if err != nil {
		return httpchat.Key{}, false, err
	}
	class, ok := apiKeyClass(key.Class)
	if !ok {
		return httpchat.Key{}, false, httpchat.Errorf(httpchat.ErrorCodeInvalidArgument, "api key %s has an unknown class", key.ID)
	}
	return httpchat.Key{
		ID: key.ID, Digest: key.KeySHA256, PersonaID: key.PersonaID, Class: class,
		CreatedAt: key.CreatedAt, Revoked: !key.Active(),
	}, true, nil
}

type httpSessionStore struct {
	sessions store.SessionService
}

func (s *httpSessionStore) Get(ctx context.Context, id string) (httpchat.Session, bool, error) {
	found, err := s.sessions.Get(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return httpchat.Session{}, false, nil
	}
	if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeSessionNotFound {
		return httpchat.Session{}, false, nil
	}
	if err != nil {
		return httpchat.Session{}, false, err
	}
	return httpchat.Session{ID: found.ID, OwnerID: found.OwnerID, PersonaID: found.AgentID}, true, nil
}

func (s *httpSessionStore) Create(ctx context.Context, session *httpchat.Session) error {
	now := time.Now().UTC()
	return s.sessions.Create(ctx, &store.Session{
		ID: session.ID, OwnerID: session.OwnerID, AgentID: session.PersonaID,
		Metadata: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now,
	})
}

func buildHTTPChat(cfg *config.Config, db *sql.DB, backend runtime.AgentRuntime, registry *auraagent.Registry, recorder *telemetry.AccessRecorder, logger *slog.Logger) (*httpchat.Server, []health.RegisteredCheck, error) {
	settings := &cfg.Channels.HTTP
	if !settings.Enabled {
		return nil, nil, nil
	}
	gates := discordAccessSettings(cfg, db, recorder)
	chat, err := httpchat.NewChat(&httpchat.ChatOptions{
		Runtime:            backend,
		Sessions:           &httpSessionStore{sessions: store.NewSessionService(db)},
		Personas:           registry,
		Budgets:            gates.Budgets,
		Evals:              gates.Evals,
		Handoffs:           gates.Handoffs,
		Denied:             gates.Denied,
		AnswerTexts:        runtimeengine.DeliveryTexts,
		MaxHistoryMessages: settings.MaxHistoryMessages,
		MaxHistoryBytes:    settings.MaxHistoryBytes,
		KeepaliveInterval:  time.Duration(settings.KeepaliveInterval),
		Logger:             logger,
	})
	if err != nil {
		return nil, nil, err
	}
	handler, err := httpchat.NewHandler(&httpchat.Options{
		Keys:                &apiKeyDirectory{keys: store.NewAPIKeyStore(db)},
		Chat:                chat,
		MaxBodyBytes:        settings.MaxBodyBytes,
		RequestsPerMinute:   settings.RequestsPerMinute,
		MaxConcurrentPerKey: settings.MaxConcurrentPerKey,
		Logger:              logger,
	})
	if err != nil {
		return nil, nil, err
	}
	server, err := httpchat.NewServer(handler, settings.ListenAddress, time.Duration(cfg.Runtime.ShutdownTimeout)/httpChatDrainDivisor, logger)
	if err != nil {
		return nil, nil, err
	}
	check := health.RegisteredCheck{
		ID:          "http",
		Checker:     server,
		Timeout:     time.Duration(cfg.Health.CheckTimeout),
		Remediation: httpchat.RemediationReviewListener,
	}
	return server, []health.RegisteredCheck{check}, nil
}
