//go:build linux

package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

func TestToolExecutorRedactsTheGitHubTokens(t *testing.T) {
	const token = "ghp-test-secret"
	t.Setenv("AURA_TEST_GITHUB_TOKEN", token)
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "note.txt"), []byte("the token is "+token), 0o600); err != nil {
		t.Fatalf("write note: %v", err)
	}
	cfg := config.Default()
	cfg.Tools.Workspace = workspace
	cfg.Storage.Path = dir
	cfg.Tools.GitHub.Credentials = []config.GitHubCredential{{Name: "ci", Ref: "env://AURA_TEST_GITHUB_TOKEN"}}
	storetest.Seed(t, filepath.Join(dir, "aura.db"))
	db, err := store.OpenDB(t.Context(), filepath.Join(dir, "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	executor, err := newToolExecutor(&toolExecutorInput{
		result: loadResultWithAll(t, &cfg), db: db, artifactRoot: filepath.Join(dir, "artifacts"), logger: slog.Default(),
		wiring: &toolWiring{},
	})
	if err != nil {
		t.Fatalf("newToolExecutor: %v", err)
	}
	output, err := executor.Execute(context.Background(), &runtimeadk.BuiltinToolRequest{
		RequestID: "request-1", TurnID: "turn-1", SessionID: "session-1", PrincipalID: "owner-1",
		ToolName: "read_file", ToolVersion: "v1", Arguments: json.RawMessage(`{"path":"note.txt"}`),
		Capabilities: []string{"workspace-read"}, Trust: string(approval.TrustDerivedUntrusted),
		IdempotencyKey: "test/read", EventSequence: 1, EventInvocation: "inv-1", EventBranch: "main", EventAuthor: "agent",
	})
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if strings.Contains(string(output), token) || !strings.Contains(string(output), "the token is") {
		t.Errorf("read_file output = %s, want the note with the token redacted", output)
	}
}

func TestWorkflowToolRunnerRefusesAnUnconfiguredGitHubCredential(t *testing.T) {
	comment := func(credential string) json.RawMessage {
		return json.RawMessage(`{"repo":"org/repo","number":7,"body":"hi","credential":"` + credential + `"}`)
	}
	runner, _, _ := newWorkflowToolRunnerFixtureWith(t, func(cfg *config.Config) {
		cfg.Tools.GitHub.Credentials = []config.GitHubCredential{{Name: "ci", Ref: "env://AURA_TEST_GITHUB_TOKEN"}}
	})
	_, err := runner.Invoke(stepCallContext(t), github.ToolComment, comment("other"))
	if err == nil || !strings.Contains(err.Error(), "not a configured github credential") {
		t.Errorf("unconfigured credential err = %v, want not a configured github credential", err)
	}
	_, err = runner.Invoke(stepCallContext(t), github.ToolComment, comment("ci"))
	if err == nil || !strings.Contains(err.Error(), "github credential is unavailable") {
		t.Errorf("configured credential err = %v, want github credential is unavailable", err)
	}
	bare, _, _ := newWorkflowToolRunnerFixture(t)
	_, err = bare.Invoke(stepCallContext(t), github.ToolComment, comment("ci"))
	if code, ok := toolbroker.CodeOf(err); !ok || code != toolbroker.ResultPolicyDenied || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("no credentials err = %v, want policy_denied unknown tool", err)
	}
}
