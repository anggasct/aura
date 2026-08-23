package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"
)

func TestBuiltinToolExecutorComposesBrokerAdapters(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "note.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	db, err := store.OpenDB(context.Background(), filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := config.Default()
	cfg.Tools.Workspace = workspace
	cfg.Storage.Path = filepath.Dir(filepath.Join(t.TempDir(), "aura.db"))
	executor, err := newBuiltinToolExecutor(&cfg, db, nil)
	if err != nil {
		t.Fatalf("newBuiltinToolExecutor: %v", err)
	}
	definitions := executor.Definitions()
	if len(definitions) != 6 {
		t.Fatalf("builtin definitions = %d, want 6", len(definitions))
	}
	wantDefinitions := []string{"exec@v1", "list_dir@v1", "read_file@v1", "web_fetch@v1", "web_search@v1", "write_file@v1"}
	for i, definition := range definitions {
		if key := definition.Name + "@" + definition.Version; key != wantDefinitions[i] {
			t.Errorf("definition[%d] = %q, want %q", i, key, wantDefinitions[i])
		}
	}
	output, err := executor.Execute(context.Background(), &runtime.BuiltinToolRequest{
		RequestID:    "call-1",
		TurnID:       "turn-1",
		SessionID:    "session-1",
		PrincipalID:  "owner-1",
		ToolName:     "read_file",
		ToolVersion:  "v1",
		Arguments:    json.RawMessage(`{"path":"note.txt"}`),
		Capabilities: []string{"workspace-read"},
		Trust:        string(approval.TrustDerivedUntrusted),
	})
	if err != nil {
		t.Fatalf("execute read_file: %v", err)
	}
	var result struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.Content != "hello" {
		t.Fatalf("content = %q, want hello", result.Content)
	}
}
