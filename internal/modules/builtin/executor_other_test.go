//go:build !linux

package toolsbuiltin

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/store"
)

func TestBuiltinToolExecutorFailsClosedWithoutLinux(t *testing.T) {
	db, err := store.OpenDB(context.Background(), filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	toolsCfg := config.DefaultTools()
	toolsCfg.Workspace = t.TempDir()
	dataRoot := t.TempDir()
	_, err = New(toolsCfg, testArtifactQuota, db, dataRoot, capabilitytest.All(t), nil, nil, nil, nil)
	if err == nil {
		t.Fatal("New succeeded on a platform without race-safe filesystem access")
	}
	if !strings.Contains(err.Error(), "requires Linux") {
		t.Fatalf("error = %v, want the Linux containment requirement", err)
	}
}
