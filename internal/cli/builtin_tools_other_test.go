//go:build !linux

package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/store"
)

// The race-safe filesystem adapters require Linux, so the executor
// constructor must fail closed on other platforms instead of composing a
// partial tool set.
func TestBuiltinToolExecutorFailsClosedWithoutLinux(t *testing.T) {
	db, err := store.OpenDB(context.Background(), filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := config.Default()
	cfg.Tools.Workspace = t.TempDir()
	cfg.Storage.Path = t.TempDir()
	_, err = newBuiltinToolExecutor(&cfg, db, nil)
	if err == nil {
		t.Fatal("newBuiltinToolExecutor succeeded on a platform without race-safe filesystem access")
	}
	if !strings.Contains(err.Error(), "requires Linux") {
		t.Fatalf("error = %v, want the Linux containment requirement", err)
	}
}
