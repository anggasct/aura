package app

import (
	"database/sql"
	"log/slog"

	"go.opentelemetry.io/otel/metric"

	appconfig "github.com/anggasct/aura/internal/app/config"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/health"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
)

func buildChannelAdapters(cfg *appconfig.Config, router *auraagent.Router, db *sql.DB, artifactRoot string, mp metric.MeterProvider, runs *erasureRuns, logger *slog.Logger) ([]runtimechannelhost.ChannelPort, []health.RegisteredCheck, []approvalAdapter, error) {
	built, err := buildServerChannels(cfg, router, db, artifactRoot, mp, runs, nil, logger)
	if err != nil {
		return nil, nil, nil, err
	}
	return built.ports, built.checks, built.approvers, nil
}
