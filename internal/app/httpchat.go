package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/channels/httpchat"
	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/modules/handoff"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/telemetry"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeengine "github.com/anggasct/aura/internal/platform/turn/engine"
	"github.com/anggasct/aura/internal/platform/turn/store"
)

const httpChatDrainDivisor = 2

type apiKeyDirectory struct {
	keys principal.APIKeyStore
}

func (d *apiKeyDirectory) GetByID(ctx context.Context, id string) (httpchat.Key, bool, error) {
	key, err := d.keys.GetByID(ctx, id)
	if code, ok := fault.CodeOf[principal.ErrorCode](err); ok && code == principal.ErrorCodeAPIKeyNotFound {
		return httpchat.Key{}, false, nil
	}
	if err != nil {
		return httpchat.Key{}, false, err
	}
	class, ok := apiKeyClass(key.Class)
	if !ok {
		return httpchat.Key{}, false, fault.New(httpchat.ErrorCodeInvalidArgument, fmt.Sprintf("api key %s has an unknown class", key.ID))
	}
	return httpchat.Key{
		ID: key.ID, Digest: key.KeySHA256, PersonaID: key.PersonaID, Class: class,
		CreatedAt: key.CreatedAt, Revoked: !key.Active(),
	}, true, nil
}

type httpSessionStore struct {
	sessions turnstore.SessionService
}

func (s *httpSessionStore) Get(ctx context.Context, id string) (httpchat.Session, bool, error) {
	found, err := s.sessions.Get(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return httpchat.Session{}, false, nil
	}
	if code, ok := fault.CodeOf[turnstore.ErrorCode](err); ok && code == turnstore.ErrorCodeSessionNotFound {
		return httpchat.Session{}, false, nil
	}
	if err != nil {
		return httpchat.Session{}, false, err
	}
	return httpchat.Session{ID: found.ID, OwnerID: found.OwnerID, PersonaID: found.AgentID}, true, nil
}

func (s *httpSessionStore) Create(ctx context.Context, session *httpchat.Session) error {
	now := time.Now().UTC()
	return s.sessions.Create(ctx, &turn.Session{
		ID: session.ID, OwnerID: session.OwnerID, AgentID: session.PersonaID,
		Metadata: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now,
	})
}

type httpHandoffReplies struct {
	replies handoff.ReplyStore
}

func (r *httpHandoffReplies) List(ctx context.Context, sessionID string, afterSequence uint64, limit int) ([]httpchat.HandoffReply, error) {
	stored, err := r.replies.List(ctx, sessionID, afterSequence, limit)
	if err != nil {
		return nil, err
	}
	replies := make([]httpchat.HandoffReply, 0, len(stored))
	for i := range stored {
		replies = append(replies, httpchat.HandoffReply{Sequence: stored[i].Sequence, Text: stored[i].Text, CreatedAt: stored[i].CreatedAt})
	}
	return replies, nil
}

func buildHTTPChat(cfg *appconfig.Config, db *sql.DB, backend turn.AgentRuntime, registry *auraagent.Registry, recorder *telemetry.AccessRecorder, approvals *approval.OwnerRoute, logger *slog.Logger) (*httpchat.Server, []health.RegisteredCheck, error) {
	settings := &cfg.Channels.HTTP
	if !settings.Enabled {
		return nil, nil, nil
	}
	replyStore, err := handoff.NewReplyStore(turnstore.NewTurnStore(db))
	if err != nil {
		return nil, nil, err
	}
	server, err := httpchat.New(&httpchat.Deps{
		Config:        settings,
		Keys:          &apiKeyDirectory{keys: principal.NewAPIKeyStore(db)},
		Sessions:      &httpSessionStore{sessions: turnstore.NewSessionService(db)},
		Replies:       &httpHandoffReplies{replies: replyStore},
		Personas:      registry,
		Runtime:       backend,
		Access:        channelAccessSettings(cfg, db, recorder, nil),
		Approvals:     approvals,
		AnswerTexts:   runtimeengine.DeliveryTexts,
		ShutdownGrace: time.Duration(cfg.Runtime.ShutdownTimeout) / httpChatDrainDivisor,
		Logger:        logger,
	})
	if err != nil {
		return nil, nil, err
	}
	checks := server.HealthChecks()
	for i := range checks {
		if checks[i].Timeout == 0 {
			checks[i].Timeout = time.Duration(cfg.Health.CheckTimeout)
		}
	}
	return server, checks, nil
}
