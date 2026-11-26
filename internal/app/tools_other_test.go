//go:build !linux

package app

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/turn"
)

func TestBuiltinToolExecutorFailsClosedWithoutLinux(t *testing.T) {
	db, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "aura.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := appconfig.Default()
	cfg.Tools.Workspace = t.TempDir()
	_, err = newTestToolExecutor(&toolExecutorInput{
		result:       &appconfig.LoadResult{Config: &cfg, CapabilityReport: capabilitytest.All(t)},
		db:           db,
		artifactRoot: t.TempDir(),
		logger:       slog.New(slog.DiscardHandler),
		wiring:       &toolWiring{},
		events:       turn.NewEventBus(slog.New(slog.DiscardHandler)),
	})
	if err == nil {
		t.Fatal("newTestToolExecutor succeeded on a platform without race-safe filesystem access")
	}
	if !strings.Contains(err.Error(), "requires Linux") {
		t.Fatalf("error = %v, want the Linux containment requirement", err)
	}
}
