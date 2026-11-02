package cli

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/channel/discord"
	"github.com/anggasct/aura/internal/workflow"
)

type fakeOfferer struct {
	mu       sync.Mutex
	sessions map[string]bool
	offered  []string
	decide   discord.ProposalDecider
	fail     error
	attempts int
}

func (f *fakeOfferer) OfferProposal(_ context.Context, proposalID, plan string, decide discord.ProposalDecider) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.fail != nil {
		return f.fail
	}
	f.offered = append(f.offered, proposalID+"|"+plan)
	f.decide = decide
	return nil
}

func (f *fakeOfferer) waitAttempts(t *testing.T, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		attempts := f.attempts
		f.mu.Unlock()
		if attempts >= count {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d card offers", count)
}

func (f *fakeOfferer) ReplyTarget(sessionID string) (discord.HandoffTarget, bool) {
	return discord.HandoffTarget{Account: "front", ChannelID: "333"}, f.sessions[sessionID]
}

func TestProposalCardsOfferTheBoundAccountAndDecideThroughTheCLIPath(t *testing.T) {
	h := proposalFixture(t)
	other := &fakeOfferer{}
	bound := &fakeOfferer{sessions: map[string]bool{h.owner.SessionID: true}}
	h.controller.enableProposalCards(h.cfg, []proposalOfferer{other, bound}, slog.New(slog.DiscardHandler))
	go h.controller.runProposalCards(t.Context())
	proposed, err := h.controller.Propose(t.Context(), h.owner, json.RawMessage(coveredProposal))
	if err != nil {
		t.Fatal(err)
	}
	bound.waitAttempts(t, 1)
	if len(other.offered) != 0 || len(bound.offered) != 1 || !strings.HasPrefix(bound.offered[0], proposed.ProposalID+"|Plan: Write the release note") {
		t.Fatalf("offers: other=%v bound=%v", other.offered, bound.offered)
	}
	reply, err := bound.decide(t.Context(), true)
	if err != nil || !strings.HasPrefix(reply, "approved "+proposed.ProposalID+": run ") {
		t.Fatalf("approve from the card = %q, %v", reply, err)
	}
	approved, err := h.runs.Proposal(t.Context(), proposed.ProposalID, time.Now())
	if err != nil || approved.Status != workflow.ProposalApproved || approved.RunID == "" {
		t.Fatalf("proposal after the card approval = %+v, %v", approved, err)
	}
	if _, err := bound.decide(t.Context(), true); !hasWorkflowCode(err, workflow.ErrorCodeProposalDecided) {
		t.Fatalf("a second card decision = %v", err)
	}

	second, err := h.controller.Propose(t.Context(), h.owner, json.RawMessage(coveredProposal))
	if err != nil {
		t.Fatal(err)
	}
	bound.waitAttempts(t, 2)
	if reply, err := bound.decide(t.Context(), false); err != nil || reply != "rejected "+second.ProposalID {
		t.Fatalf("reject from the card = %q, %v", reply, err)
	}
	if rejected, _ := h.runs.Proposal(t.Context(), second.ProposalID, time.Now()); rejected.Status != workflow.ProposalRejected {
		t.Fatalf("proposal after the card rejection = %+v", rejected)
	}
}

func TestProposalIsKeptWhenNoCardCanBeOffered(t *testing.T) {
	h := proposalFixture(t)
	failing := &fakeOfferer{fail: errors.New("owner has no discord identity")}
	h.controller.enableProposalCards(h.cfg, []proposalOfferer{failing}, slog.New(slog.DiscardHandler))
	go h.controller.runProposalCards(t.Context())
	proposed, err := h.controller.Propose(t.Context(), h.owner, json.RawMessage(coveredProposal))
	if err != nil {
		t.Fatal(err)
	}
	failing.waitAttempts(t, 1)
	if stored, err := h.runs.Proposal(t.Context(), proposed.ProposalID, time.Now()); err != nil || stored.Status != workflow.ProposalProposed {
		t.Fatalf("proposal without a card = %+v, %v", stored, err)
	}
}
