package cli

import (
	"context"
	"database/sql"
	"log/slog"

	"github.com/anggasct/aura/internal/channel/telegram"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/store"
	toolsbuiltin "github.com/anggasct/aura/internal/tools/builtin"
)

type telegramAdapters []*telegram.Adapter

func (adapters telegramAdapters) Check(ctx context.Context) []health.Finding {
	findings := make([]health.Finding, 0, len(adapters))
	for _, adapter := range adapters {
		findings = append(findings, adapter.Check(ctx)...)
	}
	return findings
}

func newTelegramAdapter(cfg *config.Config, account *config.TelegramAccount, db *sql.DB, logger *slog.Logger) (*telegram.Adapter, error) {
	executor, err := toolsbuiltin.NewChannelEffects(db, logger)
	if err != nil {
		return nil, err
	}
	adapter, err := telegram.New(&cfg.Channels.Telegram, account, store.NewTelegramCursorStore(db), executor, &discordSessionEnsurer{sessions: store.NewSessionService(db)}, logger)
	if err != nil {
		return nil, err
	}
	adapter.SetSessionResolver(&discordConversationResolver{pointers: store.NewConversationSessionStore(db)})
	return adapter, nil
}
