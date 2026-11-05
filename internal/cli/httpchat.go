package cli

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/anggasct/aura/internal/channel/httpchat"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/store"
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

func buildHTTPChat(cfg *config.Config, db *sql.DB, logger *slog.Logger) (*httpchat.Server, []health.RegisteredCheck, error) {
	settings := &cfg.Channels.HTTP
	if !settings.Enabled {
		return nil, nil, nil
	}
	handler, err := httpchat.NewHandler(&httpchat.Options{
		Keys:                &apiKeyDirectory{keys: store.NewAPIKeyStore(db)},
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
