package app

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/channels/email"
	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/channels/kit/media"
	"github.com/anggasct/aura/internal/config"
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/storage"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/store"
)

type emailCursors struct {
	cursors store.EmailCursorStore
}

func (c emailCursors) Load(ctx context.Context, source string) (email.Cursor, bool, error) {
	cursor, found, err := c.cursors.Load(ctx, source)
	if err != nil || !found {
		return email.Cursor{}, false, err
	}
	return email.Cursor{UIDValidity: cursor.UIDValidity, LastUID: cursor.LastUID}, true, nil
}

func (c emailCursors) Save(ctx context.Context, source string, cursor email.Cursor, at time.Time) error {
	return c.cursors.Save(ctx, source, store.EmailCursor{UIDValidity: cursor.UIDValidity, LastUID: cursor.LastUID}, at)
}

type emailAdapters []*email.Adapter

func (adapters emailAdapters) Check(ctx context.Context) []health.Finding {
	findings := make([]health.Finding, 0, len(adapters))
	for _, adapter := range adapters {
		findings = append(findings, adapter.Check(ctx)...)
	}
	return findings
}

func newEmailAdapter(cfg *appconfig.Config, account *config.EmailAccount, db *sql.DB, artifactRoot string, logger *slog.Logger) (*email.Adapter, error) {
	connector, err := email.NewIMAPConnector(account)
	if err != nil {
		return nil, err
	}
	mailer, err := email.NewSMTPMailer(account)
	if err != nil {
		return nil, err
	}
	executor, err := toolsbuiltin.NewChannelEffects(db, logger)
	if err != nil {
		return nil, err
	}
	sessions, err := access.NewSessionEnsurer(turnstore.NewSessionService(db))
	if err != nil {
		return nil, err
	}
	mediaStore, err := media.NewArtifactStore(storage.NewArtifactStore(db, artifactRoot, int64(cfg.Storage.ArtifactQuota)))
	if err != nil {
		return nil, err
	}
	adapter, err := email.New(&cfg.Channels.Email, account, connector, mailer, emailCursors{cursors: store.NewEmailCursorStore(db)}, executor, sessions, logger)
	if err != nil {
		return nil, err
	}
	adapter.SetMediaStore(mediaStore)
	if svc := newVisionService(cfg, db, artifactRoot); svc != nil {
		adapter.SetVisionService(svc)
	}
	return adapter, nil
}

type emailOutbox struct {
	adapters []*email.Adapter
}

var _ toolsbuiltin.EmailOutbox = (*emailOutbox)(nil)

func (o *emailOutbox) adapterFor(sessionID string) (*email.Adapter, email.Route, bool) {
	for _, adapter := range o.adapters {
		if route, ok := adapter.SessionRoute(sessionID); ok {
			return adapter, route, true
		}
	}
	return nil, email.Route{}, false
}

func (o *emailOutbox) RouteOf(sessionID string) (toolsbuiltin.EmailRoute, bool) {
	_, route, ok := o.adapterFor(sessionID)
	return toolsbuiltin.EmailRoute{Account: route.Account, Sender: route.Sender}, ok
}

func (o *emailOutbox) Send(ctx context.Context, message *toolsbuiltin.EmailMessage) (string, error) {
	adapter, _, ok := o.adapterFor(message.SessionID)
	if !ok {
		return "", toolbroker.Errorf(toolbroker.ResultCapabilityUnavailable, "this session has no email account to send from")
	}
	messageID, err := adapter.Send(ctx, &email.Outgoing{
		SessionID:      message.SessionID,
		IdempotencyKey: message.IdempotencyKey,
		To:             message.To,
		Subject:        message.Subject,
		Body:           message.Body,
	})
	if err == nil {
		return messageID, nil
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	switch code, _ := email.CodeOf(err); code {
	case email.ErrorCodeMessageInvalid, email.ErrorCodeInvalidArgument:
		return "", toolbroker.Errorf(toolbroker.ResultInvalidArgument, "the message cannot be sent as written")
	case email.ErrorCodeDeliveryAmbiguous:
		return "", toolbroker.Errorf(toolbroker.ResultExecutionFailed, "the delivery outcome is unknown and the message was not resent")
	default:
		return "", toolbroker.Errorf(toolbroker.ResultExecutionFailed, "email could not be sent")
	}
}

func recoverEmailEffects(ctx context.Context, cfg *appconfig.Config, db *sql.DB, logger *slog.Logger) error {
	if !cfg.Channels.Email.Enabled {
		return nil
	}
	executor, err := toolsbuiltin.NewChannelEffects(db, logger)
	if err != nil {
		return err
	}
	report, err := executor.Journal().RecoverProvider(ctx, email.ProviderName)
	if err != nil {
		return fmt.Errorf("recover email effects: %w", err)
	}
	logger.InfoContext(ctx, "email effects recovered", "component", "email", "scanned", report.Scanned, "claimed", report.Claimed)
	return nil
}
