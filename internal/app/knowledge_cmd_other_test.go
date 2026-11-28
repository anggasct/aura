//go:build !linux

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/platform/storage"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

func TestKBRefreshFailsClosedOnWorkspaceFilesWithoutLinux(t *testing.T) {
	cfgPath := writeMemoryCLIConfig(t)
	cfg, db, _ := kbTestStorage(t, cfgPath)
	service, err := newKnowledgeService(cfg, db, nil)
	if err != nil {
		t.Fatalf("newKnowledgeService: %v", err)
	}
	now := time.Now().UTC()
	if err := turnstore.NewSessionService(db).Create(t.Context(), &turn.Session{ID: "session-1", OwnerID: "owner-1", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create the session: %v", err)
	}
	_, artifactRoot, _, err := storage.ResolvePaths(&cfg.Storage)
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	artifacts := storage.NewArtifactStore(db, artifactRoot, int64(cfg.Storage.ArtifactQuota))
	attachment, err := artifacts.Put(t.Context(), strings.NewReader("# Faq\nHow do refunds work?\n"), &storage.ArtifactMetadata{
		ID: "art-0123456789abcdef", SessionID: "session-1", Filename: "faq.md", MediaType: "text/markdown",
	})
	if err != nil {
		t.Fatalf("store the attachment: %v", err)
	}
	workspaceFile := filepath.Join(cfg.Tools.Workspace, "docs", "policy.md")
	if err := os.MkdirAll(filepath.Dir(workspaceFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workspaceFile, []byte("# Policy\nRefunds are issued within thirty days.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, stored := range []struct{ uri, name, body string }{
		{"workspace:docs/policy.md", "policy.md", "# Policy\nRefunds are issued within thirty days.\n"},
		{"artifact:" + attachment.ID, "faq.md", "# Faq\nHow do refunds work?\n"},
	} {
		prepared, err := service.PrepareStream(t.Context(), "support", &knowledge.Stream{
			URI: stored.uri, Name: stored.name, MediaType: "text/markdown", Size: int64(len(stored.body)), Reader: strings.NewReader(stored.body),
		})
		if err != nil {
			t.Fatalf("prepare %s: %v", stored.uri, err)
		}
		if _, err := service.Write(t.Context(), knowledge.ActionAdd, prepared); err != nil {
			t.Fatalf("write %s: %v", stored.uri, err)
		}
	}

	out, err := runKBCommand(t, cfgPath, "refresh", "support")

	if err != nil {
		t.Fatalf("refresh: %v (%s)", err, out)
	}
	if !strings.Contains(out, "failed: policy.md (source_unavailable)") || !strings.Contains(out, "unchanged: faq.md") {
		t.Fatalf("refresh output = %q, want the workspace file refused and the attachment unchanged", out)
	}
	_, err = runKBCommand(t, cfgPath, "refresh", "support", "workspace:docs/policy.md")
	requireKBCode(t, err, knowledge.ErrorCodeNothingRefreshed)
	if !strings.Contains(err.Error(), "policy.md (source_unavailable)") {
		t.Errorf("error %q does not name the refused workspace source", err.Error())
	}
	show, err := runKBCommand(t, cfgPath, "show", "support")
	if err != nil || !strings.Contains(show, "workspace:docs/policy.md") || !strings.Contains(show, "artifact:"+attachment.ID) {
		t.Fatalf("show = %q, %v; want both stored sources kept", show, err)
	}
}
