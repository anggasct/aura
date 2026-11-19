package app

import (
	"testing"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
)

func loadResultWithAll(tb testing.TB, cfg *appconfig.Config) *appconfig.LoadResult {
	tb.Helper()
	return &appconfig.LoadResult{Config: cfg, CapabilityReport: capabilitytest.All(tb)}
}
