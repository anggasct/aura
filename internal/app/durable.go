package app

import (
	"database/sql"
	"log/slog"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/broadcast"
	"github.com/anggasct/aura/internal/modules/child"
	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/durable/sqlite"
)

func durableRuntimeForConfig(cfg *config.Config, db *sql.DB, logger *slog.Logger) (durable.Runtime, error) {
	if cfg == nil || db == nil {
		return nil, invalidOptions("the durable runtime needs a config and open storage")
	}
	runtime, err := sqlite.New(durableHostOptions(db, logger, cfg.Runtime))
	if err != nil {
		return nil, err
	}
	declareRunHandlers(runtime)
	return runtime, nil
}

type runHandlerDeclarer interface {
	DeclareHandlers(names ...string)
}

func declareRunHandlers(runtime runHandlerDeclarer) {
	runtime.DeclareHandlers(broadcast.HandlerName, schedule.HandlerName, child.HandlerName, workflow.HandlerName)
}
