package app

import (
	"testing"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
)

func loadResultWithAll(tb testing.TB, cfg *config.Config) *config.LoadResult {
	tb.Helper()
	return &config.LoadResult{Config: cfg, CapabilityReport: capabilitytest.All(tb)}
}
