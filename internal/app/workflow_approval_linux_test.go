//go:build linux

package app

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/store"
)

func githubProposalFixture(t *testing.T) *proposalHarness {
	t.Helper()
	h := proposalFixture(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Storage.Path = dir
	cfg.Tools.Workspace = filepath.Join(dir, "workspace")
	if err := os.MkdirAll(cfg.Tools.Workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.Tools.GitHub.Credentials = []config.GitHubCredential{{Name: "ci", Ref: "env://AURA_TEST_GITHUB_TOKEN"}}
	cfg.Agents = &config.Agents{Definitions: []config.AgentDefinition{{
		ID: "ops", Description: "operator", Instructions: "You plan work.", Tools: []string{"github_comment", "workflow_propose"},
	}}}
	registry, err := buildAgentRegistry(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	deps, err := workflowValidationDeps(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.controller.interpreter.LoadAll(t.Context(), nil, deps); err != nil {
		t.Fatal(err)
	}
	executor, err := newToolExecutor(&toolExecutorInput{
		result: loadResultWithAll(t, &cfg), db: h.db, artifactRoot: filepath.Join(dir, "artifacts"), logger: slog.New(slog.DiscardHandler),
		wiring: &toolWiring{},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.controller.registry = registry
	h.controller.enableCallDisplay(executor.Broker().CallDisplay)
	return h
}

func commentProposal(t *testing.T, body string) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	args := `{"repo":"org/repo","number":7,"body":` + string(encoded) + `,"credential_name":"ci"}`
	return json.RawMessage(`{"goal":"Comment on the pull request","steps":[
	{"id":"approve","executor":{"kind":"approval"},"timeout":60000000000},
	{"id":"comment","depends_on":["approve"],"executor":{"kind":"tool","tool":"github_comment","args":` + args + `},"timeout":60000000000}]}`)
}

func TestProposalPlanUsesTheToolsApprovalDisplay(t *testing.T) {
	t.Run("the plan shows the call target first", func(t *testing.T) {
		h := githubProposalFixture(t)
		proposed, err := h.controller.Propose(t.Context(), h.owner, commentProposal(t, "looks good"))
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Join([]string{
			"2. comment — run tool github_comment (after approve)",
			"    repo: org/repo",
			"    number: 7",
			"    credential_name: ci",
			"    body: looks good",
		}, "\n")
		if !strings.Contains(proposed.Plan, want) {
			t.Fatalf("plan lacks the call block:\n%s\nin:\n%s", want, proposed.Plan)
		}
	})
	t.Run("a call that does not fit the display is refused at propose time", func(t *testing.T) {
		h := githubProposalFixture(t)
		_, err := h.controller.Propose(t.Context(), h.owner, commentProposal(t, strings.Repeat("x", 2000)))
		var rejection *toolsbuiltin.WorkflowRejection
		if !errors.As(err, &rejection) || rejection.Code != string(workflow.ErrorCodePlanUnshowable) || !strings.Contains(rejection.Detail, `step "comment"`) {
			t.Fatalf("Propose = %v, want a %s rejection naming the step", err, workflow.ErrorCodePlanUnshowable)
		}
		if got := proposalCount(t, h.runs); got != 0 {
			t.Fatalf("%d proposals stored for an unshowable plan, want none", got)
		}
	})
}

func TestProposalShowRendersTheToolCallsOrRefusesThePlan(t *testing.T) {
	tests := []struct {
		name       string
		configured bool
		want       string
	}{
		{name: "tools configured", configured: true, want: "    {\"content\":\"hi\",\"path\":\"note.md\"}"},
		{name: "no tools configured"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gf := writeWorkflowFixtures(t, validWorkflowYAML)
			if tc.configured {
				workspace := filepath.Join(filepath.Dir(gf.configPath), "workspace")
				if err := os.MkdirAll(workspace, 0o700); err != nil {
					t.Fatal(err)
				}
				configured, err := os.ReadFile(gf.configPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(gf.configPath, append(configured, []byte("tools:\n  workspace: "+workspace+"\n")...), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			tool := "write_file"
			args, err := json.Marshal(map[string]string{"path": "note.md", "content": "hi"})
			if err != nil {
				t.Fatal(err)
			}
			spec := &workflow.Spec{
				ID: "gen_shown", Goal: "Show the plan", Version: 1, Source: workflow.SourceGenerated,
				Steps: []workflow.StepSpec{
					{ID: "approve", Executor: workflow.ExecutorSpec{Kind: workflow.KindApproval}, Timeout: time.Minute},
					{ID: "write", DependsOn: []string{"approve"}, Executor: workflow.ExecutorSpec{Kind: workflow.KindTool, ToolID: &tool, ToolArgs: args}, Timeout: time.Minute},
				},
			}
			digest, err := workflow.Digest(spec)
			if err != nil {
				t.Fatal(err)
			}
			proposal := &workflow.Proposal{
				ID: workflow.NewProposalID(), SessionID: "ops/terminal:owner", AgentID: "ops", PrincipalID: principal.OwnerID,
				Spec: spec, Digest: digest, CreatedAt: time.Now().UTC(),
			}
			db := openAccessDB(t, gf.configPath)
			if err := store.NewSessionService(db).Create(t.Context(), &store.Session{ID: proposal.SessionID, OwnerID: principal.OwnerID, Metadata: []byte(`{}`), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			if err := workflow.NewStore(db).CreateProposal(t.Context(), proposal); err != nil {
				t.Fatal(err)
			}
			out, err := runWorkflowCommand(t, gf, "proposal", "show", proposal.ID)
			if !tc.configured {
				if !hasWorkflowCode(err, workflow.ErrorCodePlanUnshowable) {
					t.Fatalf("proposal show = %v, want %s", err, workflow.ErrorCodePlanUnshowable)
				}
				return
			}
			if err != nil || !strings.Contains(out, tc.want) {
				t.Fatalf("proposal show = %q, %v, want %q", out, err, tc.want)
			}
		})
	}
}
