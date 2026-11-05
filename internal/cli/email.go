package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/anggasct/aura/internal/channel/email"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/store"
	toolsbuiltin "github.com/anggasct/aura/internal/tools/builtin"
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

type emailSessions struct {
	discordSessionEnsurer
}

func (s *emailSessions) SessionOwner(ctx context.Context, sessionID string) (ownerID string, found bool, err error) {
	if s.sessions == nil {
		return "", false, nil
	}
	session, err := s.sessions.Get(ctx, sessionID)
	if err == nil {
		return session.OwnerID, true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeSessionNotFound {
		return "", false, nil
	}
	return "", false, err
}

type emailAdapters []*email.Adapter

func (adapters emailAdapters) Check(ctx context.Context) []health.Finding {
	findings := make([]health.Finding, 0, len(adapters))
	for _, adapter := range adapters {
		findings = append(findings, adapter.Check(ctx)...)
	}
	return findings
}

func newEmailAdapter(cfg *config.Config, account *config.EmailAccount, db *sql.DB, logger *slog.Logger) (*email.Adapter, error) {
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
	return email.New(&cfg.Channels.Email, account, connector, mailer, emailCursors{cursors: store.NewEmailCursorStore(db)}, executor, &emailSessions{discordSessionEnsurer{sessions: store.NewSessionService(db)}}, logger)
}

func recoverEmailEffects(ctx context.Context, cfg *config.Config, db *sql.DB, logger *slog.Logger) error {
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
