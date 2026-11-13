package discord

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	policy "github.com/anggasct/aura/internal/platform/approval"
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
	if err := adapter.OfferProposal(t.Context(), ProposalCard{ID: "prop_1", Plan: "Plan: Write the release note\n1. approve — owner approval"}, recorded.decide); err != nil {
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
	if err := adapter.OfferProposal(t.Context(), ProposalCard{ID: "prop_2", Plan: "Plan: x"}, recorded.decide); err != nil {
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
	if err := adapter.OfferProposal(t.Context(), ProposalCard{ID: "prop_3", Plan: "Plan: x"}, (&recordedDecision{}).decide); err == nil {
		t.Fatal("a proposal card was offered without an owner identity")
	}
	if posts := stub.postsTo("555"); len(posts) != 0 {
		t.Fatalf("posts = %d", len(posts))
	}
}

func TestCardDoesNotEscapeThePlanTwice(t *testing.T) {
	stub := newApprovalStub(t)
	adapter, _ := newOwnerRoutedAdapter(t, stub, []string{"discord:777"}, principal.ClassOwner)
	fence := strings.Repeat("`", 3)
	plan := "Plan: ship" + fence + "[ok](https://evil)\n1. approve - owner approval\n    path: a\\\\b"
	if err := adapter.OfferProposal(t.Context(), ProposalCard{ID: "prop_1", Plan: plan}, (&recordedDecision{}).decide); err != nil {
		t.Fatal(err)
	}
	card := stub.waitPostTo(t, "555", 1)[0]
	content, _ := card.body["content"].(string)
	if flags, _ := card.body["flags"].(float64); flags != 4 {
		t.Errorf("flags = %v, want 4", card.body["flags"])
	}
	if count := strings.Count(content, fence); count != 2 {
		t.Errorf("card holds %d runs of three backticks, want 2: %q", count, content)
	}
	if !strings.Contains(content, `a\\b`) || strings.Contains(content, `a\\\\b`) {
		t.Errorf("card escapes the plan a second time: %q", content)
	}
	block := strings.TrimSuffix(strings.TrimPrefix(content[strings.Index(content, "\n")+1:], fence+"\n"), "\n"+fence)
	for _, line := range []string{"Plan: ship", "[ok](https://evil)", "1. approve - owner approval"} {
		if !strings.Contains(block, line) {
			t.Errorf("block lacks %q: %q", line, block)
		}
	}
}

func TestLongPlanCardHasNoButtons(t *testing.T) {
	cases := []struct {
		name        string
		plan        string
		wantButtons bool
	}{
		{name: "a plan over the message limit", plan: "Plan: " + strings.Repeat("a", 2100)},
		{name: "backticks that the fence pushes over the limit", plan: strings.Repeat("`", 1900)},
		{name: "a short plan", plan: "Plan: ship\n1. approve - owner approval", wantButtons: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newApprovalStub(t)
			adapter, _ := newOwnerRoutedAdapter(t, stub, []string{"discord:777"}, principal.ClassOwner)
			if err := adapter.OfferProposal(t.Context(), ProposalCard{ID: "prop_9", Plan: tc.plan}, (&recordedDecision{}).decide); err != nil {
				t.Fatal(err)
			}
			post := stub.waitPostTo(t, "555", 1)[0]
			content, _ := post.body["content"].(string)
			components, _ := post.body["components"].([]any)
			if tc.wantButtons {
				if len(components) == 0 || adapter.approvals.Len() != 1 {
					t.Fatalf("short plan: components = %v, pending tokens = %d, want buttons and one token", post.body["components"], adapter.approvals.Len())
				}
				return
			}
			if len(components) != 0 {
				t.Errorf("long plan carries components: %v", post.body["components"])
			}
			if got := adapter.approvals.Len(); got != 0 {
				t.Errorf("long plan minted %d approval tokens, want none", got)
			}
			if !strings.Contains(content, "workflow proposal prop_9 is too long for a card") || !strings.Contains(content, "aura workflow proposal show prop_9") {
				t.Errorf("notice = %q", content)
			}
			if len([]rune(content)) > discordMessageLimit {
				t.Errorf("notice is %d runes", len([]rune(content)))
			}
		})
	}
}

func TestUntrustedLineSurvivesALongPlan(t *testing.T) {
	stub := newApprovalStub(t)
	adapter, _ := newOwnerRoutedAdapter(t, stub, []string{"discord:777"}, principal.ClassOwner)
	plan := "Plan: " + strings.Repeat("a", 1794)
	if err := adapter.OfferProposal(t.Context(), ProposalCard{ID: "prop_5", Plan: plan, Tainted: true}, (&recordedDecision{}).decide); err != nil {
		t.Fatal(err)
	}
	content, _ := stub.waitPostTo(t, "555", 1)[0].body["content"].(string)
	lines := strings.Split(content, "\n")
	if len(lines) < 3 || lines[0] != "workflow proposal prop_5" || lines[1] != policy.UntrustedInputNotice {
		t.Fatalf("card head = %q, want the header then the untrusted-input line", lines[:min(len(lines), 2)])
	}
	if !strings.Contains(content, plan) {
		t.Errorf("card does not hold the whole plan")
	}
}
