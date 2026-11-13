package app

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/modules/workflow"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/store"
)

func TestApprovedProposalRunKeepsTheProposalTaint(t *testing.T) {
	tests := []struct {
		name    string
		tainted bool
	}{
		{name: "untainted proposal", tainted: false},
		{name: "tainted proposal", tainted: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := proposalFixture(t)
			owner := *h.owner
			owner.Tainted = tc.tainted
			proposed, err := h.controller.Propose(t.Context(), &owner, json.RawMessage(coveredProposal))
			if err != nil {
				t.Fatal(err)
			}
			runID, err := approveProposal(t.Context(), h.cfg, h.controller.interpreter, h.runs, proposed.ProposalID, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			run, err := h.runs.Run(t.Context(), runID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Origin.Tainted != tc.tainted {
				t.Fatalf("run tainted = %v, want %v", run.Origin.Tainted, tc.tainted)
			}
		})
	}
}

func TestCLIAndScheduledStartsRunAsTheOwnerDefaultPersona(t *testing.T) {
	t.Run("cli start", func(t *testing.T) {
		t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
		gf := writeWorkflowFixtures(t, validWorkflowYAML)
		out, err := runWorkflowCommand(t, gf, "start", "demo")
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		var runID string
		for _, line := range strings.Split(out, "\n") {
			if id, ok := strings.CutPrefix(line, "run: "); ok {
				runID = id
			}
		}
		if runID == "" {
			t.Fatalf("start output = %q, want a run id", out)
		}
		run, err := workflow.NewStore(openAccessDB(t, gf.configPath)).Run(t.Context(), runID)
		if err != nil {
			t.Fatal(err)
		}
		want := workflow.RunOrigin{PrincipalID: principal.OwnerID, PrincipalClass: "owner", AgentID: auraagent.DefaultID}
		if run.Origin != want || !run.Origin.Complete() {
			t.Fatalf("CLI run origin = %+v, want %+v", run.Origin, want)
		}
	})
	scheduled := []struct {
		name      string
		principal string
		class     string
	}{
		{name: "owner job", principal: principal.OwnerID, class: "owner"},
		{name: "other principal job", principal: "discord:111", class: "trusted"},
	}
	for _, tc := range scheduled {
		t.Run("scheduled start/"+tc.name, func(t *testing.T) {
			controller, runs, _ := workflowToolFixture(t, "book_visit")
			workflows := &scheduleWorkflows{}
			workflows.set(controller.interpreter)
			turns := &engineTurnRunner{workflows: workflows}
			fired, err := turns.RunTurn(t.Context(), &schedule.TurnRequest{PrincipalID: tc.principal, IdempotencyKey: "cron/job-1/2026-11-13T03:00:00Z", WorkflowID: "book_visit"})
			if err != nil || !fired.Succeeded {
				t.Fatalf("fire = %+v, %v", fired, err)
			}
			run, err := runs.Run(t.Context(), fired.TurnID)
			if err != nil {
				t.Fatal(err)
			}
			want := workflow.RunOrigin{PrincipalID: tc.principal, PrincipalClass: tc.class, AgentID: auraagent.DefaultID}
			if run.Origin != want || !run.Origin.Complete() {
				t.Fatalf("scheduled run origin = %+v, want %+v", run.Origin, want)
			}
		})
	}
}

func TestProposalCardAndShowSayUntrustedInput(t *testing.T) {
	tests := []struct {
		name       string
		tainted    bool
		wantTaint  string
		wantNotice bool
	}{
		{name: "untainted proposal", tainted: false, wantTaint: "tainted: no", wantNotice: false},
		{name: "tainted proposal", tainted: true, wantTaint: "tainted: yes", wantNotice: true},
	}
	for _, tc := range tests {
		t.Run("card/"+tc.name, func(t *testing.T) {
			h := proposalFixture(t)
			bound := &fakeOfferer{sessions: map[string]bool{h.owner.SessionID: true}}
			h.controller.enableProposalCards(h.cfg, []proposalOfferer{bound}, slog.New(slog.DiscardHandler))
			go h.controller.runProposalCards(t.Context())
			owner := *h.owner
			owner.Tainted = tc.tainted
			proposed, err := h.controller.Propose(t.Context(), &owner, json.RawMessage(coveredProposal))
			if err != nil {
				t.Fatal(err)
			}
			bound.waitAttempts(t, 1)
			bound.mu.Lock()
			card := strings.Join(bound.offered, "\n")
			bound.mu.Unlock()
			if strings.Contains(card, approval.UntrustedInputNotice) != tc.wantNotice {
				t.Fatalf("card carries the notice = %v, want %v:\n%s", !tc.wantNotice, tc.wantNotice, card)
			}
			if strings.Contains(proposed.Plan, approval.UntrustedInputNotice) != tc.wantNotice {
				t.Fatalf("plan carries the notice = %v, want %v:\n%s", !tc.wantNotice, tc.wantNotice, proposed.Plan)
			}
			if tc.wantNotice && !strings.Contains(card, "\n"+approval.UntrustedInputNotice+"\n") {
				t.Fatalf("the notice is not on its own line:\n%s", card)
			}
		})
		t.Run("show/"+tc.name, func(t *testing.T) {
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
				Tainted: tc.tainted, Spec: spec, Digest: digest, CreatedAt: time.Now().UTC(),
			}
			db := openAccessDB(t, gf.configPath)
			if err := store.NewSessionService(db).Create(t.Context(), &store.Session{ID: proposal.SessionID, OwnerID: principal.OwnerID, Metadata: []byte(`{}`), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			if err := workflow.NewStore(db).CreateProposal(t.Context(), proposal); err != nil {
				t.Fatal(err)
			}
			out, err := runWorkflowCommand(t, gf, "proposal", "show", proposal.ID)
			if err != nil {
				t.Fatalf("proposal show: %v", err)
			}
			if !strings.Contains(out, tc.wantTaint) {
				t.Fatalf("show output lacks %q:\n%s", tc.wantTaint, out)
			}
			if strings.Contains(out, approval.UntrustedInputNotice) != tc.wantNotice {
				t.Fatalf("show carries the notice = %v, want %v:\n%s", !tc.wantNotice, tc.wantNotice, out)
			}
			if tc.wantNotice && !strings.Contains(out, "\n"+approval.UntrustedInputNotice+"\n") {
				t.Fatalf("the notice is not on its own line:\n%s", out)
			}
		})
	}
}
