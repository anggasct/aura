package discord

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/principal"
)

type recordedDecision struct {
	mu        sync.Mutex
	decisions []bool
	reply     string
	err       error
}

func (r *recordedDecision) decide(_ context.Context, approved bool) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.decisions = append(r.decisions, approved)
	return r.reply, r.err
}

func (r *recordedDecision) snapshot() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bool(nil), r.decisions...)
}

func waitDecisions(t *testing.T, recorded *recordedDecision, count int) []bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := recorded.snapshot(); len(got) >= count {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d proposal decisions", count)
	return nil
}

func TestProposalCardIsDecidedOnlyByTheOwnerInTheDM(t *testing.T) {
	stub := newApprovalStub(t)
	adapter, _ := newOwnerRoutedAdapter(t, stub, []string{"discord:777"}, principal.ClassOwner)
	recorded := &recordedDecision{reply: "approved prop_1: run run_9"}
	if err := adapter.OfferProposal(t.Context(), "prop_1", "Plan: Write the release note\n1. approve — owner approval", recorded.decide); err != nil {
		t.Fatal(err)
	}
	card := stub.waitPostTo(t, "555", 1)[0]
	content, _ := card.body["content"].(string)
	if !strings.Contains(content, "workflow proposal prop_1") || !strings.Contains(content, "Plan: Write the release note") {
		t.Fatalf("card content = %q", content)
	}
	labels, ids := cardButtons(t, card)
	if strings.Join(labels, "|") != "Approve|Reject" {
		t.Fatalf("buttons = %v, want approve and reject only", labels)
	}
	adapter.resolveComponentClick(t.Context(), clickPayload("ix-stranger", "111", "", "555", ids[0]))
	adapter.resolveComponentClick(t.Context(), clickPayload("ix-other-channel", "777", "", "333", ids[0]))
	time.Sleep(100 * time.Millisecond)
	if got := recorded.snapshot(); len(got) != 0 {
		t.Fatalf("a click that is not the owner's in the DM decided: %v", got)
	}
	adapter.resolveComponentClick(t.Context(), clickPayload("ix-owner", "777", "", "555", ids[0]))
	if got := waitDecisions(t, recorded, 1); !got[0] {
		t.Fatalf("decisions = %v, want an approval", got)
	}
	reply := stub.waitPostTo(t, "555", 2)[1]
	if text, _ := reply.body["content"].(string); text != "approved prop_1: run run_9" {
		t.Fatalf("dm reply = %q", text)
	}
	adapter.resolveComponentClick(t.Context(), clickPayload("ix-owner-again", "777", "", "555", ids[1]))
	time.Sleep(100 * time.Millisecond)
	if got := recorded.snapshot(); len(got) != 1 {
		t.Fatalf("a decided card was decided again: %v", got)
	}
}

func TestProposalCardRejectionAndFailureAreReported(t *testing.T) {
	stub := newApprovalStub(t)
	adapter, _ := newOwnerRoutedAdapter(t, stub, []string{"discord:777"}, principal.ClassOwner)
	recorded := &recordedDecision{err: errors.New("workflow_proposal_digest_mismatch: changed")}
	if err := adapter.OfferProposal(t.Context(), "prop_2", "Plan: x", recorded.decide); err != nil {
		t.Fatal(err)
	}
	_, ids := cardButtons(t, stub.waitPostTo(t, "555", 1)[0])
	adapter.resolveComponentClick(t.Context(), clickPayload("ix-owner-reject", "777", "", "555", ids[1]))
	if got := waitDecisions(t, recorded, 1); got[0] {
		t.Fatalf("decisions = %v, want a rejection", got)
	}
	reply := stub.waitPostTo(t, "555", 2)[1]
	if text, _ := reply.body["content"].(string); text != "proposal prop_2 could not be decided: workflow_proposal_digest_mismatch: changed" {
		t.Fatalf("dm reply = %q", text)
	}
}

func TestProposalCardNeedsAnOwnerIdentity(t *testing.T) {
	stub := newApprovalStub(t)
	adapter, _ := newOwnerRoutedAdapter(t, stub, nil, principal.ClassOwner)
	if err := adapter.OfferProposal(t.Context(), "prop_3", "Plan: x", (&recordedDecision{}).decide); err == nil {
		t.Fatal("a proposal card was offered without an owner identity")
	}
	if posts := stub.postsTo("555"); len(posts) != 0 {
		t.Fatalf("posts = %d", len(posts))
	}
}
