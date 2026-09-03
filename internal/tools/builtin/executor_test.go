//go:build linux

package toolsbuiltin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/runtime/adk"
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
	dataRoot := filepath.Dir(filepath.Join(t.TempDir(), "aura.db"))
	cfg.Storage.Path = dataRoot
	var observations []toolbroker.Observation
	executor, err := New(&cfg, db, dataRoot, nil, func(_ context.Context, observation toolbroker.Observation) {
		observations = append(observations, observation)
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
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
	output, err := executor.Execute(context.Background(), &runtimeadk.BuiltinToolRequest{
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
	dataRoot := filepath.Dir(filepath.Join(t.TempDir(), "aura.db"))
	cfg.Storage.Path = dataRoot
	executor, err := New(&cfg, db, dataRoot, nil, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	output, err := executor.Execute(context.Background(), &runtimeadk.BuiltinToolRequest{
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

func TestBuiltinToolExecutorRedactsConfiguredSecretAcrossBoundaries(t *testing.T) {
	const canary = "tool-secret-7f3a9d1c"
	t.Setenv("AURA_TEST_TOOL_SECRET", canary)
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "secret.txt"), []byte(strings.Repeat(canary, 128)), 0o600); err != nil {
		t.Fatalf("write secret fixture: %v", err)
	}
	dataRoot := t.TempDir()
	db, err := store.OpenDB(context.Background(), filepath.Join(dataRoot, "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	session := store.Session{ID: "session-secret", OwnerID: "owner-1"}
	if err := store.NewSessionService(db).Create(context.Background(), &session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	cfg := config.Default()
	cfg.Tools.Workspace = workspace
	cfg.Tools.WebSearch.CredentialRef = "env://AURA_TEST_TOOL_SECRET"
	cfg.Tools.MaxInlineResultBytes = 512
	cfg.Storage.Path = dataRoot
	var prompted *toolbroker.ApprovalPrompt
	executor, err := New(&cfg, db, dataRoot, nil, nil, func(_ context.Context, prompt *toolbroker.ApprovalPrompt) (bool, error) {
		prompted = prompt
		return false, nil
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = executor.Execute(context.Background(), &runtimeadk.BuiltinToolRequest{
		RequestID:       "approval-secret",
		TurnID:          "turn-secret",
		SessionID:       "session-secret",
		PrincipalID:     "owner-1",
		ToolName:        "write_file",
		ToolVersion:     "v1",
		Arguments:       json.RawMessage(`{"path":"out.txt","content":"tool-secret-7f3a9d1c"}`),
		Capabilities:    []string{"workspace-write"},
		Trust:           string(approval.TrustOwnerInput),
		IdempotencyKey:  "approval-secret-1",
		EventSequence:   1,
		EventInvocation: "inv-secret",
		EventBranch:     "main",
		EventAuthor:     "agent",
	})
	class, ok := toolbroker.CodeOf(err)
	if !ok || class != toolbroker.ResultApprovalRequired {
		t.Fatalf("approval error = %v, want approval_required", err)
	}
	if prompted == nil || strings.Contains(prompted.Arguments, canary) || !strings.Contains(prompted.Arguments, "[REDACTED]") {
		t.Fatalf("approval prompt leaked configured secret: %+v", prompted)
	}

	diagnosticExecutor, err := New(&cfg, db, dataRoot, nil, nil, func(_ context.Context, _ *toolbroker.ApprovalPrompt) (bool, error) {
		return false, errors.New("approval surface failed: " + canary)
	})
	if err != nil {
		t.Fatalf("new diagnostic executor: %v", err)
	}
	_, err = diagnosticExecutor.Execute(context.Background(), &runtimeadk.BuiltinToolRequest{
		RequestID:      "diagnostic-secret",
		TurnID:         "turn-secret",
		SessionID:      "session-secret",
		PrincipalID:    "owner-1",
		ToolName:       "write_file",
		ToolVersion:    "v1",
		Arguments:      json.RawMessage(`{"path":"out.txt","content":"safe"}`),
		Capabilities:   []string{"workspace-write"},
		Trust:          string(approval.TrustOwnerInput),
		IdempotencyKey: "diagnostic-secret-1",
		EventSequence:  2,
	})
	if err == nil {
		t.Fatal("diagnostic executor unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("diagnostic leaked configured secret: %v", err)
	}

	output, err := executor.Execute(context.Background(), &runtimeadk.BuiltinToolRequest{
		RequestID:      "output-secret",
		TurnID:         "turn-secret",
		SessionID:      "session-secret",
		PrincipalID:    "owner-1",
		ToolName:       "read_file",
		ToolVersion:    "v1",
		Arguments:      json.RawMessage(`{"path":"secret.txt"}`),
		Capabilities:   []string{"workspace-read"},
		Trust:          string(approval.TrustDerivedUntrusted),
		IdempotencyKey: "output-secret-1",
	})
	if err != nil {
		t.Fatalf("read secret fixture: %v", err)
	}
	var envelope struct {
		ArtifactID string `json:"artifact_id"`
	}
	if err := json.Unmarshal(output, &envelope); err != nil {
		t.Fatalf("decode output envelope: %v", err)
	}
	if envelope.ArtifactID == "" {
		t.Fatalf("expected persisted output artifact, got %s", output)
	}
	artifact, _, err := store.NewArtifactStore(db, dataRoot, int64(cfg.Storage.ArtifactQuota)).Open(context.Background(), envelope.ArtifactID)
	if err != nil {
		t.Fatalf("open persisted output: %v", err)
	}
	defer func() { _ = artifact.Close() }()
	persisted, err := io.ReadAll(artifact)
	if err != nil {
		t.Fatalf("read persisted output: %v", err)
	}
	if strings.Contains(string(persisted), canary) {
		t.Fatalf("persisted tool output leaked configured secret: %s", persisted)
	}
}
