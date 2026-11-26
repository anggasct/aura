package app

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	appconfig "github.com/anggasct/aura/internal/app/config"
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/modules/workflow"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

type proposalHarness struct {
	controller  *workflow.Controller
	interpreter *workflow.Interpreter
	runs        *workflow.Store
	registry    *auraagent.Registry
	showCall    workflow.ToolCallText
	cfg         *appconfig.Config
	owner       *workflow.Caller
	db          *sql.DB
}

func (h *proposalHarness) rebuildController(t *testing.T, offerers ...workflow.ProposalOfferer) {
	t.Helper()
	controller, err := workflow.NewController(&workflow.ControllerDeps{
		Interpreter: h.interpreter,
		Runs:        h.runs,
		Registry:    h.registry,
		Offerers:    offerers,
		ShowCall:    h.showCall,
		Logger:      slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	h.controller = controller
}

func proposalFixture(t *testing.T) *proposalHarness {
	t.Helper()
	base, runs, db := workflowToolFixture(t, "book_visit")
	cfg := &appconfig.Config{Agents: &auraagent.Config{Definitions: []auraagent.Persona{{
		ID: "ops", Description: "operator", Instructions: "You plan work.", Tools: []string{"read_file", "write_file", "workflow_propose"},
	}}}}
	registry, err := buildAgentRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	interpreter := base.Interpreter()
	deps := interpreter.ValidationDeps()
	deps.Agents = registry
	deps.StepAllowed = stepCeiling(registry, toolsbuiltin.NewStepCatalog(nil))
	if err := interpreter.LoadAll(t.Context(), nil, deps); err != nil {
		t.Fatal(err)
	}
	owner := &workflow.Caller{SessionID: "ops/terminal:owner", PrincipalID: principal.OwnerID, PrincipalClass: principal.ClassOwner, AgentID: "ops"}
	if err := turnstore.NewSessionService(db).Create(t.Context(), &turn.Session{ID: owner.SessionID, OwnerID: principal.OwnerID, Metadata: []byte(`{}`), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	h := &proposalHarness{
		interpreter: interpreter,
		runs:        runs,
		registry:    registry,
		showCall: func(toolID string, args json.RawMessage) (string, error) {
			return "call " + toolID + " " + string(args), nil
		},
		cfg:   cfg,
		owner: owner,
		db:    db,
	}
	h.rebuildController(t)
	return h
}

const coveredProposal = `{"goal":"Write the release note","steps":[
	{"id":"approve","executor":{"kind":"approval"},"timeout":60000000000},
	{"id":"write","depends_on":["approve"],"executor":{"kind":"tool","tool":"write_file","args":{"path":"note.md","content":"hi"}},"timeout":60000000000},
	{"id":"ci","depends_on":["write"],"executor":{"kind":"wait","event":"ci.done"},"timeout":60000000000}]}`

func proposalCount(t *testing.T, runs *workflow.Store) int {
	t.Helper()
	proposals, err := runs.Proposals(t.Context(), "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return len(proposals)
}

func TestProposeStoresAValidatedPlanForTheOwnerOnly(t *testing.T) {
	h := proposalFixture(t)
	controller, runs, owner := h.controller, h.runs, h.owner
	proposed, err := controller.Propose(t.Context(), owner, json.RawMessage(coveredProposal))
	if err != nil {
		t.Fatal(err)
	}
	if len(proposed.Digest) != 64 || !strings.HasPrefix(proposed.ProposalID, "prop") {
		t.Fatalf("proposal = %+v", proposed)
	}
	for _, want := range []string{"Plan: Write the release note", "owner approval", "run tool write_file (after approve)", "wait for ci.done"} {
		if !strings.Contains(proposed.Plan, want) {
			t.Fatalf("plan lacks %q:\n%s", want, proposed.Plan)
		}
	}
	stored, err := runs.Proposal(t.Context(), proposed.ProposalID, time.Now())
	if err != nil || stored.Status != workflow.ProposalProposed || stored.Digest != proposed.Digest || stored.Spec.ID != "gen_"+proposed.ProposalID || stored.Spec.Source != workflow.SourceGenerated {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
	for _, class := range []principal.Class{principal.ClassTrusted, principal.ClassPublic} {
		guest := *owner
		guest.PrincipalClass, guest.PrincipalID = class, "discord:111"
		if _, err := controller.Propose(t.Context(), &guest, json.RawMessage(coveredProposal)); !fault.HasCode(err, workflow.ErrorCodeNotPermitted) {
			t.Fatalf("%s propose = %v, want workflow_not_permitted", class, err)
		}
	}
	if got := proposalCount(t, runs); got != 1 {
		t.Fatalf("proposals stored = %d", got)
	}
}

func TestInvalidProposalsAreRejectedAndNothingIsStored(t *testing.T) {
	h := proposalFixture(t)
	controller, runs, owner := h.controller, h.runs, h.owner
	for label, tc := range map[string]struct {
		spec string
		code workflow.ErrorCode
	}{
		"tool outside the persona": {spec: `{"goal":"g","steps":[{"id":"a","executor":{"kind":"approval"},"timeout":60000000000},{"id":"run","depends_on":["a"],"executor":{"kind":"tool","tool":"exec","args":{"command":["true"]}},"timeout":60000000000}]}`, code: workflow.ErrorCodeCapabilityExceeded},
		"uncovered dangerous tool": {spec: `{"goal":"g","steps":[{"id":"write","executor":{"kind":"tool","tool":"write_file","args":{"path":"a","content":"b"}},"timeout":60000000000}]}`, code: workflow.ErrorCodeDangerousUncovered},
		"unknown child workflow":   {spec: `{"goal":"g","steps":[{"id":"sub","executor":{"kind":"workflow","workflow":"ghost"},"timeout":60000000000}]}`, code: workflow.ErrorCodeCompositionInvalid},
		"not a workflow spec":      {spec: `{"goal":"g","steps":[],"shell":"rm -rf /"}`, code: workflow.ErrorCodeSpecInvalid},
	} {
		_, err := controller.Propose(t.Context(), owner, json.RawMessage(tc.spec))
		if !fault.HasCode(err, tc.code) {
			t.Fatalf("%s: err = %v, want %s", label, err, tc.code)
		}
	}
	if got := proposalCount(t, runs); got != 0 {
		t.Fatalf("rejected proposals were stored: %d", got)
	}
}

func TestApprovalRunsExactlyTheApprovedDigest(t *testing.T) {
	h := proposalFixture(t)
	controller, runs, owner := h.controller, h.runs, h.owner
	proposed, err := controller.Propose(t.Context(), owner, json.RawMessage(coveredProposal))
	if err != nil {
		t.Fatal(err)
	}
	runID, err := workflow.ApproveRenderedProposal(t.Context(), controller.Interpreter(), runs, proposed.ProposalID, controller.ShowCall(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	run, err := runs.Run(t.Context(), runID)
	if err != nil || run.DefinitionID != "gen_"+proposed.ProposalID || run.Origin.SessionID != owner.SessionID || run.Origin.PrincipalID != principal.OwnerID {
		t.Fatalf("run = %+v, %v", run, err)
	}
	approved, err := runs.Proposal(t.Context(), proposed.ProposalID, time.Now())
	if err != nil || approved.Status != workflow.ProposalApproved || approved.RunID != runID {
		t.Fatalf("approved proposal = %+v, %v", approved, err)
	}
	if _, err := workflow.ApproveRenderedProposal(t.Context(), controller.Interpreter(), runs, proposed.ProposalID, controller.ShowCall(), time.Now()); !hasWorkflowCode(err, workflow.ErrorCodeProposalDecided) {
		t.Fatalf("second approval = %v", err)
	}
}

func TestTamperedProposalIsRefusedAndRejectionRunsNothing(t *testing.T) {
	h := proposalFixture(t)
	controller, runs, owner := h.controller, h.runs, h.owner
	proposed, err := controller.Propose(t.Context(), owner, json.RawMessage(coveredProposal))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(t.Context(), `UPDATE workflow_proposal SET spec_json = replace(spec_json, 'note.md', 'passwd') WHERE id = ?`, proposed.ProposalID); err != nil {
		t.Fatal(err)
	}
	if _, err := workflow.ApproveRenderedProposal(t.Context(), controller.Interpreter(), runs, proposed.ProposalID, controller.ShowCall(), time.Now()); !hasWorkflowCode(err, workflow.ErrorCodeDigestMismatch) {
		t.Fatalf("approving a modified spec = %v", err)
	}
	second, err := controller.Propose(t.Context(), owner, json.RawMessage(coveredProposal))
	if err != nil {
		t.Fatal(err)
	}
	if err := runs.DecideProposal(t.Context(), second.ProposalID, workflow.ProposalRejected, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	all, err := runs.Runs(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range all {
		if strings.HasPrefix(run.DefinitionID, "gen_") {
			t.Fatalf("a refused or rejected proposal ran: %+v", run)
		}
	}
	if _, ok := controller.Interpreter().Spec("gen_" + proposed.ProposalID); ok {
		t.Fatal("a tampered proposal was loaded as a definition")
	}
}

func hasWorkflowCode(err error, want workflow.ErrorCode) bool {
	return fault.HasCode(err, want)
}

func TestWorkflowProposalShowRunsThroughTheCommand(t *testing.T) {
	gf := writeWorkflowFixtures(t, validWorkflowYAML)
	spec := &workflow.Spec{
		ID: "gen_shown", Goal: "Show the plan", Version: 1, Source: workflow.SourceGenerated,
		Steps: []workflow.StepSpec{{ID: "approve", Executor: workflow.ExecutorSpec{Kind: workflow.KindApproval}, Timeout: time.Minute}},
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
	if err := turnstore.NewSessionService(db).Create(t.Context(), &turn.Session{ID: proposal.SessionID, OwnerID: principal.OwnerID, Metadata: []byte(`{}`), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := mustWorkflowStore(t, db).CreateProposal(t.Context(), proposal); err != nil {
		t.Fatal(err)
	}
	out, err := runWorkflowCommand(t, gf, "proposal", "show", proposal.ID)
	if err != nil {
		t.Fatalf("proposal show: %v", err)
	}
	if !strings.Contains(out, "proposal: "+proposal.ID) {
		t.Fatalf("output = %q, want the proposal id", out)
	}
}

func TestWorkflowProposalsInvalidStatusExitsWithCode2(t *testing.T) {
	restoreDefaultLogger(t)
	root := NewRootCommand()
	args := []string{"workflow", "proposals", "--status", "invalid"}
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(args)
	err := root.ExecuteContext(t.Context())
	if err == nil {
		t.Fatal("proposals --status invalid succeeded")
	}
	if got := ExitCode(root, args, err); got != 2 {
		t.Fatalf("ExitCode = %d, want 2 (err=%v)", got, err)
	}
}

func TestProposalResultSeparatesPlanAndHint(t *testing.T) {
	for _, tainted := range []bool{false, true} {
		name := "untainted"
		if tainted {
			name = "tainted"
		}
		t.Run(name, func(t *testing.T) {
			h := proposalFixture(t)
			owner := *h.owner
			owner.Tainted = tainted
			proposed, err := h.controller.Propose(t.Context(), &owner, json.RawMessage(coveredProposal))
			if err != nil {
				t.Fatal(err)
			}
			stored, err := h.runs.Proposal(t.Context(), proposed.ProposalID, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			graph, err := workflow.Compile(stored.Spec, h.controller.Interpreter().ValidationDeps())
			if err != nil {
				t.Fatal(err)
			}
			wantPlan, err := workflow.RenderPlan(stored.Spec, graph, h.controller.ShowCall())
			if err != nil {
				t.Fatal(err)
			}
			if proposed.Plan != wantPlan {
				t.Fatalf("plan:\n%s\nwant the rendered plan:\n%s", proposed.Plan, wantPlan)
			}
			if proposed.UntrustedInput != tainted {
				t.Fatalf("untrusted_input = %v, want %v", proposed.UntrustedInput, tainted)
			}
			if want := "aura workflow proposal approve " + proposed.ProposalID; proposed.ApproveWith != want {
				t.Fatalf("approve_with = %q, want %q", proposed.ApproveWith, want)
			}
			for _, unwanted := range []string{approval.UntrustedInputNotice, "Approve with", "aura workflow proposal approve"} {
				if strings.Contains(proposed.Plan, unwanted) {
					t.Fatalf("the plan carries %q:\n%s", unwanted, proposed.Plan)
				}
			}
		})
	}
}
