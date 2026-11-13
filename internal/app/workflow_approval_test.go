package app

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/principal"
)

func TestApprovedProposalRecordsTheApprover(t *testing.T) {
	approvals := []string{"cli", "card"}
	for _, via := range approvals {
		for _, tainted := range []bool{false, true} {
			name := via + "/untainted"
			if tainted {
				name = via + "/tainted"
			}
			t.Run(name, func(t *testing.T) {
				h := proposalFixture(t)
				owner := *h.owner
				owner.Tainted = tainted
				bound := &fakeOfferer{sessions: map[string]bool{h.owner.SessionID: true}}
				h.controller.enableProposalCards(h.cfg, []proposalOfferer{bound}, slog.New(slog.DiscardHandler))
				go h.controller.runProposalCards(t.Context())
				proposed, err := h.controller.Propose(t.Context(), &owner, json.RawMessage(coveredProposal))
				if err != nil {
					t.Fatal(err)
				}
				bound.waitAttempts(t, 1)
				if via == "cli" {
					_, err = approveProposal(t.Context(), h.cfg, h.controller.interpreter, h.runs, proposed.ProposalID, time.Now())
				} else {
					_, err = bound.decide(t.Context(), true)
				}
				if err != nil {
					t.Fatal(err)
				}
				record, err := h.runs.Approval(t.Context(), "gen_"+proposed.ProposalID, 1)
				if err != nil {
					t.Fatal(err)
				}
				want := workflow.Approval{
					DefinitionID: "gen_" + proposed.ProposalID, DefinitionVersion: 1, Digest: proposed.Digest, Kind: workflow.ApprovalProposal,
					AgentID: "ops", ApproverID: principal.OwnerID, ProposalID: proposed.ProposalID, Tainted: tainted,
				}
				record.ApprovedAt = time.Time{}
				if *record != want {
					t.Fatalf("approval record = %+v, want %+v", *record, want)
				}
			})
		}
	}
}
