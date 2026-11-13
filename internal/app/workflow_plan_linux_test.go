//go:build linux

package app

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/workflow"
)

func toolPlanSpec(t *testing.T, toolID string, args any) *workflow.Spec {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return &workflow.Spec{
		ID: "gen_plan_shown", Goal: "Show the plan", Version: 1, Source: workflow.SourceGenerated,
		Steps: []workflow.StepSpec{
			{ID: "approve", Executor: workflow.ExecutorSpec{Kind: workflow.KindApproval}, Timeout: time.Minute},
			{ID: "call", DependsOn: []string{"approve"}, Executor: workflow.ExecutorSpec{Kind: workflow.KindTool, ToolID: &toolID, ToolArgs: encoded}, Timeout: time.Minute},
		},
	}
}

func TestPlanRendersCallsExactlyOrNotAtAll(t *testing.T) {
	const secret = "sk-canary-9f2a"
	cases := []struct {
		name string
		tool string
		args map[string]any
	}{
		{name: "a write_file whose content exceeds the display budget", tool: "write_file", args: map[string]any{"path": "note.md", "content": strings.Repeat("c", 3000)}},
		{name: "an email_send whose body would be tail-cut", tool: "email_send", args: map[string]any{"to": "a@mail.example", "subject": "Notes", "body": strings.Repeat("b", 3000)}},
		{name: "an email_send holding a configured secret", tool: "email_send", args: map[string]any{"to": "a@mail.example", "subject": "Notes", "body": "token " + secret}},
		{name: "an exec holding a configured secret", tool: "exec", args: map[string]any{"command": []string{"echo", secret}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := githubProposalFixture(t)
			h.secrets(secret)
			spec := toolPlanSpec(t, tc.tool, tc.args)
			graph, err := workflow.Compile(spec, h.controller.interpreter.ValidationDeps())
			if err != nil {
				t.Fatal(err)
			}
			plan, err := workflow.RenderPlan(spec, graph, h.controller.showCall)
			if !hasWorkflowCode(err, workflow.ErrorCodePlanUnshowable) || plan != "" {
				t.Fatalf("RenderPlan = %q, %v, want no plan and %s", plan, err, workflow.ErrorCodePlanUnshowable)
			}
		})
	}
}

func TestApproveRefusesAnUnshowablePlan(t *testing.T) {
	const body = "looks good to me"
	for _, via := range []string{"cli", "card"} {
		t.Run(via, func(t *testing.T) {
			h := githubProposalFixture(t)
			bound := &fakeOfferer{sessions: map[string]bool{h.owner.SessionID: true}}
			h.controller.enableProposalCards(h.cfg, []proposalOfferer{bound}, slog.New(slog.DiscardHandler))
			go h.controller.runProposalCards(t.Context())
			proposed, err := h.controller.Propose(t.Context(), h.owner, commentProposal(t, body))
			if err != nil {
				t.Fatal(err)
			}
			bound.waitAttempts(t, 1)
			h.secrets(body)
			if via == "cli" {
				_, err = approveProposal(t.Context(), h.cfg, h.controller.interpreter, h.runs, proposed.ProposalID, h.controller.showCall, time.Now())
			} else {
				_, err = bound.decide(t.Context(), true)
			}
			if !hasWorkflowCode(err, workflow.ErrorCodePlanUnshowable) {
				t.Fatalf("approval = %v, want %s", err, workflow.ErrorCodePlanUnshowable)
			}
			var approvals int
			if err := h.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM workflow_approval WHERE kind = 'proposal'`).Scan(&approvals); err != nil {
				t.Fatal(err)
			}
			runs, err := h.runs.Runs(t.Context(), "")
			if err != nil {
				t.Fatal(err)
			}
			if approvals != 0 || len(runs) != 0 {
				t.Fatalf("approval records = %d, runs = %d, want none of either", approvals, len(runs))
			}
			stored, err := h.runs.Proposal(t.Context(), proposed.ProposalID, time.Now())
			if err != nil || stored.Status != workflow.ProposalProposed {
				t.Fatalf("proposal after the refused approval = %+v, %v", stored, err)
			}
		})
	}
}
