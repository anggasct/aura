package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/channels/discord"
	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/channels/telegram"
	"github.com/anggasct/aura/internal/modules/workflow"
)

type fakeOfferer struct {
	mu       sync.Mutex
	sessions map[string]bool
	offered  []string
	cards    []approval.ProposalCard
	decide   approval.ProposalDecider
	fail     error
	attempts int
}

func (f *fakeOfferer) OfferProposal(_ context.Context, card approval.ProposalCard, decide approval.ProposalDecider) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.fail != nil {
		return f.fail
	}
	f.offered = append(f.offered, card.ID+"|"+card.Plan)
	f.cards = append(f.cards, card)
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

func (f *fakeOfferer) ReplyTarget(sessionID string) (access.HandoffTarget, bool) {
	return access.HandoffTarget{Channel: "discord", Account: "front", ChannelID: "333"}, f.sessions[sessionID]
}

func TestProposalCardsOfferTheBoundAccountAndDecideThroughTheCLIPath(t *testing.T) {
	h := proposalFixture(t)
	other := &fakeOfferer{}
	bound := &fakeOfferer{sessions: map[string]bool{h.owner.SessionID: true}}
	h.rebuildController(t, &workflowProposalOffererAdapter{offerer: other}, &workflowProposalOffererAdapter{offerer: bound})
	go h.controller.RunProposalCards(t.Context())
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
	h.rebuildController(t, &workflowProposalOffererAdapter{offerer: failing})
	go h.controller.RunProposalCards(t.Context())
	proposed, err := h.controller.Propose(t.Context(), h.owner, json.RawMessage(coveredProposal))
	if err != nil {
		t.Fatal(err)
	}
	failing.waitAttempts(t, 1)
	if stored, err := h.runs.Proposal(t.Context(), proposed.ProposalID, time.Now()); err != nil || stored.Status != workflow.ProposalProposed {
		t.Fatalf("proposal without a card = %+v, %v", stored, err)
	}
}

func TestProposalOfferersFollowTheChannelOrderOfTheBuiltPorts(t *testing.T) {
	db := discordTestDB(t)
	cfg := enabledDiscordConfig()
	telegramCfg := enabledTelegramConfig()
	cfg.Owner = telegramCfg.Owner
	cfg.Channels.Telegram = telegramCfg.Channels.Telegram
	ports, _, _, err := buildChannelAdapters(&cfg, testRouter(t, &cfg), db, t.TempDir(), nil, nil, nil)
	if err != nil {
		t.Fatalf("buildChannelAdapters(): %v", err)
	}
	offerers := workflowProposalOfferers(ports)
	var kinds []string
	for _, offerer := range offerers {
		switch wrapped := offerer.(*workflowProposalOffererAdapter).offerer.(type) {
		case *discord.Adapter:
			kinds = append(kinds, "discord")
		case *telegram.Adapter:
			kinds = append(kinds, "telegram")
		default:
			t.Fatalf("offerer wraps %T", wrapped)
		}
	}
	if !slices.Equal(kinds, []string{"discord", "telegram"}) {
		t.Fatalf("offerers = %v, want discord then telegram", kinds)
	}
	if targeters := workflowReplyTargeters(ports); len(targeters) != 2 {
		t.Fatalf("reply targeters = %d, want one per port", len(targeters))
	}
}
