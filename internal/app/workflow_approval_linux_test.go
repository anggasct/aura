//go:build linux

package app

import (
	"database/sql"
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

type githubHarness struct {
	*proposalHarness
	secrets func(value string)
}

func githubProposalFixture(t *testing.T) *githubHarness {
	t.Helper()
	h := &githubHarness{proposalHarness: proposalFixture(t)}
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
	h.cfg = &cfg
	h.controller.enableCallDisplay(executor.Broker().ExactCallText)
	h.secrets = executor.Broker().RegisterSecret
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

func TestApprovalBindsToTheStoredPlanText(t *testing.T) {
	approved := func(t *testing.T, h *githubHarness, body string) *workflow.Proposal {
		t.Helper()
		proposed, err := h.controller.Propose(t.Context(), h.owner, commentProposal(t, body))
		if err != nil {
			t.Fatal(err)
		}
		stored, err := h.runs.Proposal(t.Context(), proposed.ProposalID, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if stored.PlanText == "" || stored.PlanText != proposed.Plan {
			t.Fatalf("stored plan = %q, want the proposed plan %q", stored.PlanText, proposed.Plan)
		}
		return stored
	}
	t.Run("the card path approves the plan it showed", func(t *testing.T) {
		h := githubProposalFixture(t)
		proposal := approved(t, h, "looks good")
		runID, err := approveRenderedProposal(t.Context(), h.cfg, h.controller.interpreter, h.runs, proposal.ID, h.controller.showCall, time.Now())
		if err != nil || runID == "" {
			t.Fatalf("approveRenderedProposal = %q, %v, want a run", runID, err)
		}
	})
	t.Run("the card path refuses a plan that renders differently now", func(t *testing.T) {
		h := githubProposalFixture(t)
		proposal := approved(t, h, "looks good")
		h.secrets("looks")
		_, err := approveRenderedProposal(t.Context(), h.cfg, h.controller.interpreter, h.runs, proposal.ID, h.controller.showCall, time.Now())
		if !hasWorkflowCode(err, workflow.ErrorCodePlanUnshowable) {
			t.Fatalf("approveRenderedProposal after a new secret = %v, want %s", err, workflow.ErrorCodePlanUnshowable)
		}
	})
	t.Run("the card path refuses without a renderer", func(t *testing.T) {
		h := githubProposalFixture(t)
		proposal := approved(t, h, "looks good")
		_, err := approveRenderedProposal(t.Context(), h.cfg, h.controller.interpreter, h.runs, proposal.ID, nil, time.Now())
		if !hasWorkflowCode(err, workflow.ErrorCodePlanUnshowable) {
			t.Fatalf("approveRenderedProposal without a renderer = %v, want %s", err, workflow.ErrorCodePlanUnshowable)
		}
	})
	t.Run("the command path refuses step arguments that hold a configured secret", func(t *testing.T) {
		h := githubProposalFixture(t)
		proposal := approved(t, h, "token tok-4471")
		_, err := approveStoredProposal(t.Context(), h.cfg, h.controller.interpreter, h.runs, proposal.ID, []string{"tok-4471"}, time.Now())
		if !hasWorkflowCode(err, workflow.ErrorCodePlanUnshowable) {
			t.Fatalf("approveStoredProposal with a configured secret in the arguments = %v, want %s", err, workflow.ErrorCodePlanUnshowable)
		}
		runID, err := approveStoredProposal(t.Context(), h.cfg, h.controller.interpreter, h.runs, proposal.ID, []string{"unrelated-secret"}, time.Now())
		if err != nil || runID == "" {
			t.Fatalf("approveStoredProposal without a held secret = %q, %v, want a run", runID, err)
		}
	})
	t.Run("a proposal without a stored plan is refused on both paths", func(t *testing.T) {
		h := githubProposalFixture(t)
		proposal := unplannedProposal(t, h.runs, h.db)
		if _, err := approveRenderedProposal(t.Context(), h.cfg, h.controller.interpreter, h.runs, proposal.ID, h.controller.showCall, time.Now()); !hasWorkflowCode(err, workflow.ErrorCodePlanUnshowable) {
			t.Fatalf("approveRenderedProposal of an unplanned proposal = %v, want %s", err, workflow.ErrorCodePlanUnshowable)
		}
		if _, err := approveStoredProposal(t.Context(), h.cfg, h.controller.interpreter, h.runs, proposal.ID, nil, time.Now()); !hasWorkflowCode(err, workflow.ErrorCodePlanUnshowable) {
			t.Fatalf("approveStoredProposal of an unplanned proposal = %v, want %s", err, workflow.ErrorCodePlanUnshowable)
		}
	})
	t.Run("proposal show prints the stored plan byte for byte", func(t *testing.T) {
		gf := writeWorkflowFixtures(t, validWorkflowYAML)
		db := openAccessDB(t, gf.configPath)
		const plan = "1. approve — wait for owner approval\n2. write — run tool write_file (after approve)\n    path: note.md\n    content: [redacted]"
		planned := unplannedProposal(t, workflow.NewStore(db), db, plan)
		out, err := runWorkflowCommand(t, gf, "proposal", "show", planned.ID)
		if err != nil || !strings.HasSuffix(strings.TrimRight(out, "\n"), plan) {
			t.Fatalf("proposal show = %q, %v, want the stored plan %q", out, err, plan)
		}
		unplanned := unplannedProposal(t, workflow.NewStore(db), db)
		out, err = runWorkflowCommand(t, gf, "proposal", "show", unplanned.ID)
		if err != nil || !strings.Contains(out, planNotStoredLine) {
			t.Fatalf("proposal show without a stored plan = %q, %v, want %q", out, err, planNotStoredLine)
		}
	})
}

func unplannedProposal(t *testing.T, runs *workflow.Store, db *sql.DB, plan ...string) *workflow.Proposal {
	t.Helper()
	tool := "write_file"
	args, err := json.Marshal(map[string]string{"path": "note.md", "content": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	spec := &workflow.Spec{
		ID: "gen_" + strings.ToLower(workflow.NewProposalID()[5:13]), Goal: "Show the plan", Version: 1, Source: workflow.SourceGenerated,
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
		ID: workflow.NewProposalID(), SessionID: "ops/terminal:" + spec.ID, AgentID: "ops", PrincipalID: principal.OwnerID,
		Spec: spec, PlanText: strings.Join(plan, ""), Digest: digest, CreatedAt: time.Now().UTC(),
	}
	if err := store.NewSessionService(db).Create(t.Context(), &store.Session{ID: proposal.SessionID, OwnerID: principal.OwnerID, Metadata: []byte(`{}`), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := runs.CreateProposal(t.Context(), proposal); err != nil {
		t.Fatal(err)
	}
	return proposal
}
