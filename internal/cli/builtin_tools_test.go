//go:build linux

package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/toolbroker"
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
	var observations []toolbroker.Observation
	executor, err := newBuiltinToolExecutor(&cfg, db, nil, func(observation toolbroker.Observation) {
		observations = append(observations, observation)
	})
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
	if len(observations) != 1 {
		t.Fatalf("observations = %d, want 1", len(observations))
	}
	observation := observations[0]
	if observation.ToolName != "read_file" || observation.ToolVersion != "v1" || observation.Class != toolbroker.ResultOK {
		t.Fatalf("observation = %+v", observation)
	}
	if observation.Duration <= 0 || observation.OutputBytes == 0 {
		t.Fatalf("observation lacks duration/bytes: %+v", observation)
	}
}

func TestBuiltinToolExecutorSpillsOversizedResultsToArtifacts(t *testing.T) {
	workspace := t.TempDir()
	big := strings.Repeat("a", 512)
	if err := os.WriteFile(filepath.Join(workspace, "big.txt"), []byte(big), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	db, err := store.OpenDB(context.Background(), filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sess := store.Session{ID: "session-1", OwnerID: "owner-1"}
	if err := store.NewSessionService(db).Create(context.Background(), &sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	cfg := config.Default()
	cfg.Tools.Workspace = workspace
	cfg.Tools.MaxInlineResultBytes = 256
	cfg.Storage.Path = filepath.Dir(filepath.Join(t.TempDir(), "aura.db"))
	executor, err := newBuiltinToolExecutor(&cfg, db, nil, nil)
	if err != nil {
		t.Fatalf("newBuiltinToolExecutor: %v", err)
	}
	output, err := executor.Execute(context.Background(), &runtime.BuiltinToolRequest{
		RequestID:       "call-1",
		TurnID:          "turn-1",
		SessionID:       "session-1",
		PrincipalID:     "owner-1",
		ToolName:        "read_file",
		ToolVersion:     "v1",
		Arguments:       json.RawMessage(`{"path":"big.txt"}`),
		Capabilities:    []string{"workspace-read"},
		Trust:           string(approval.TrustDerivedUntrusted),
		IdempotencyKey:  "test/spill/1",
		EventSequence:   1,
		EventInvocation: "inv-1",
		EventBranch:     "main",
		EventAuthor:     "agent",
	})
	if err != nil {
		t.Fatalf("execute read_file: %v", err)
	}
	var envelope struct {
		ArtifactID string `json:"artifact_id"`
		Digest     string `json:"digest"`
		SizeBytes  int64  `json:"size_bytes"`
		Truncated  bool   `json:"truncated"`
		Body       string `json:"body"`
	}
	if err := json.Unmarshal(output, &envelope); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, output)
	}
	if envelope.ArtifactID == "" {
		t.Fatalf("oversized result did not spill to an artifact: %s", output)
	}
	if envelope.Truncated {
		t.Fatalf("artifact spill must not be marked truncated: %s", output)
	}
	if envelope.Digest == "" || envelope.SizeBytes == 0 {
		t.Fatalf("envelope lacks digest/size: %s", output)
	}
	if envelope.Body != "" {
		t.Fatalf("artifact spill must not inline the body: %s", output)
	}
}
