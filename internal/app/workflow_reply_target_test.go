package app

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/modules/workflow"
)

const holdProposal = `{"goal":"Hold the release","steps":[{"id":"hold","executor":{"kind":"wait","event":"ci.done"},"timeout":60000000000}]}`

func (h *proposalHarness) targetOwnerSession(t *testing.T, sessions map[string]access.HandoffTarget) *fakeOfferer {
	t.Helper()
	h.targets = []workflow.ReplyTargeter{&workflowReplyTargeterAdapter{target: fixedTarget{sessions: sessions}}}
	card := &fakeOfferer{sessions: map[string]bool{h.owner.SessionID: true}}
	h.rebuildController(t, &discordProposalOffererAdapter{offerer: card})
	go h.controller.RunProposalCards(t.Context())
	return card
}

func (h *proposalHarness) approveProposal(t *testing.T, card *fakeOfferer, viaCard bool) (proposalID, runID string) {
	t.Helper()
	proposed, err := h.controller.Propose(t.Context(), h.owner, json.RawMessage(holdProposal))
	if err != nil {
		t.Fatal(err)
	}
	if viaCard {
		card.waitAttempts(t, 1)
		card.mu.Lock()
		decide := card.decide
		card.mu.Unlock()
		if _, err := decide(t.Context(), true); err != nil {
			t.Fatalf("approve from the card: %v", err)
		}
		approved, err := h.runs.Proposal(t.Context(), proposed.ProposalID, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return proposed.ProposalID, approved.RunID
	}
	runID, err = workflow.ApproveStoredProposal(t.Context(), h.interpreter, h.runs, proposed.ProposalID, nil, time.Now())
	if err != nil {
		t.Fatalf("approve from the CLI: %v", err)
	}
	return proposed.ProposalID, runID
}

func TestApprovedProposalRunInheritsTheProposingConversation(t *testing.T) {
	telegramTopic := access.HandoffTarget{Channel: "telegram", Account: "pocket", ChannelID: "-1001234567890", ThreadID: "7"}
	discordChannel := access.HandoffTarget{Channel: "discord", Account: "front", ChannelID: "333"}
	cases := []struct {
		name    string
		target  *access.HandoffTarget
		viaCard bool
		want    workflow.ReplyTarget
	}{
		{"telegram origin approved on the card", &telegramTopic, true, workflow.ReplyTarget{Channel: "telegram", Account: "pocket", ChannelID: "-1001234567890", ThreadID: "7"}},
		{"telegram origin approved with the CLI", &telegramTopic, false, workflow.ReplyTarget{Channel: "telegram", Account: "pocket", ChannelID: "-1001234567890", ThreadID: "7"}},
		{"discord origin approved on the card", &discordChannel, true, workflow.ReplyTarget{Channel: "discord", Account: "front", ChannelID: "333"}},
		{"discord origin approved with the CLI", &discordChannel, false, workflow.ReplyTarget{Channel: "discord", Account: "front", ChannelID: "333"}},
		{"session without a target approved on the card", nil, true, workflow.ReplyTarget{}},
		{"session without a target approved with the CLI", nil, false, workflow.ReplyTarget{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := proposalFixture(t)
			sessions := map[string]access.HandoffTarget{}
			if tc.target != nil {
				sessions[h.owner.SessionID] = *tc.target
			}
			card := h.targetOwnerSession(t, sessions)
			proposalID, runID := h.approveProposal(t, card, tc.viaCard)
			proposal, err := h.runs.Proposal(t.Context(), proposalID, time.Now())
			if err != nil || proposal.Reply != tc.want {
				t.Fatalf("proposal target = %+v, %v; want %+v", proposal, err, tc.want)
			}
			run, err := h.runs.Run(t.Context(), runID)
			if err != nil || run.Origin.Reply != tc.want {
				t.Fatalf("run target = %+v, %v; want %+v", run, err, tc.want)
			}
		})
	}
}

func TestApprovedProposalRunsPostTheirCompletion(t *testing.T) {
	h := proposalFixture(t)
	sessions := map[string]access.HandoffTarget{h.owner.SessionID: {Channel: "telegram", Account: "pocket", ChannelID: "-1001234567890", ThreadID: "7"}}
	card := h.targetOwnerSession(t, sessions)
	_, telegramRun := h.approveProposal(t, card, true)

	sessions[h.owner.SessionID] = access.HandoffTarget{Channel: "discord", Account: "front", ChannelID: "333"}
	_, discordRun := h.approveProposal(t, card, false)

	recorder := &deliveryRecorder{}
	notifier := newCompletionNotifier(t, h.runs, h.registry, h.db, recorder)
	finishRun(t, h.runs, telegramRun, workflow.RunSucceeded)
	finishRun(t, h.runs, discordRun, workflow.RunSucceeded)
	notifier.Sweep(t.Context())
	notifier.Sweep(t.Context())

	wantTelegram := []deliveredTelegramText{{"pocket", "-1001234567890", "7", "Done: Hold the release"}}
	if len(recorder.telegram) != 1 || recorder.telegram[0] != wantTelegram[0] {
		t.Fatalf("telegram deliveries = %+v, want %+v", recorder.telegram, wantTelegram)
	}
	wantDiscord := []deliveredText{{"front", "333", "Done: Hold the release"}}
	if len(recorder.sent) != 1 || recorder.sent[0] != wantDiscord[0] {
		t.Fatalf("discord deliveries = %+v, want %+v", recorder.sent, wantDiscord)
	}
}
