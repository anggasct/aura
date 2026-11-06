package cli

import (
	"testing"

	"github.com/anggasct/aura/internal/capability/capabilitytest"
	"github.com/anggasct/aura/internal/config"
)

func loadResultWithAll(tb testing.TB, cfg *config.Config) *config.LoadResult {
	tb.Helper()
	return &config.LoadResult{Config: cfg, CapabilityReport: capabilitytest.All(tb)}
}
