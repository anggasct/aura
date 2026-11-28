package profile

import (
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
)

func newStoredService(t *testing.T) *Service {
	t.Helper()
	store, _ := newTestSQLiteStore(t)
	service, err := NewService(store, ServiceOptions{MinConfidence: 0.70})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return service
}

func servedFacts(t *testing.T, service *Service, now time.Time) []ContextFact {
	t.Helper()
	part, err := service.Retrieve(t.Context(), &RetrieveQuery{OwnerID: "owner-1", MaxFacts: 10, MaxTokens: 1000, VerifiedOnly: true}, now)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	return part.Facts
}

func TestAnAgentProposalIsAnUnverifiedCandidateTheOwnerContextNeverServes(t *testing.T) {
	service := newStoredService(t)
	now := time.Now().UTC()

	fact, err := service.ProposeAgentFact(t.Context(), &AgentProposal{OwnerID: "owner-1", Category: "tool", Key: "editor", Value: "neovim"}, now)

	if err != nil {
		t.Fatalf("ProposeAgentFact: %v", err)
	}
	if fact.Origin != OriginAgent || fact.OwnerVerified || fact.Status != StatusCandidate {
		t.Fatalf("fact = %+v, want an unverified agent candidate", fact)
	}
	if served := servedFacts(t, service, now); len(served) != 0 {
		t.Errorf("owner context serves %+v before the owner accepted anything", served)
	}
	review, err := service.ListFacts(t.Context(), "owner-1", StatusCandidate, "", 10)
	if err != nil || len(review) != 1 || review[0].ID != fact.ID {
		t.Errorf("review = %+v, %v, want the proposal listed for the owner", review, err)
	}
}

func TestAnAgentProposalNeverShadowsAnOwnerVerifiedFact(t *testing.T) {
	service := newStoredService(t)
	now := time.Now().UTC()
	verified, err := service.SetFact(t.Context(), "owner-1", "tool", "editor", "neovim", nil, now)
	if err != nil {
		t.Fatalf("SetFact: %v", err)
	}

	rival, err := service.ProposeAgentFact(t.Context(), &AgentProposal{OwnerID: "owner-1", Category: "tool", Key: "editor", Value: "emacs"}, now)

	if err != nil {
		t.Fatalf("ProposeAgentFact: %v", err)
	}
	if rival.Status != StatusCandidate || rival.OwnerVerified || rival.ConflictsWithID != verified.ID {
		t.Fatalf("rival = %+v, want an unverified candidate that names the verified fact it conflicts with", rival)
	}
	kept, err := service.GetFact(t.Context(), "owner-1", verified.ID)
	if err != nil || kept.Status != StatusActive || !kept.OwnerVerified || kept.Value != "neovim" {
		t.Fatalf("verified fact = %+v, %v, want it untouched", kept, err)
	}
	if served := servedFacts(t, service, now); len(served) != 1 || served[0].Value != "neovim" {
		t.Errorf("owner context serves %+v, want only the verified fact", served)
	}
	if _, err := service.AcceptFact(t.Context(), "owner-1", rival.ID, now); !fault.HasCode(err, ErrorCodeProfileConflict) {
		t.Errorf("accepting the rival = %v, want %s while the verified fact holds the slot", err, ErrorCodeProfileConflict)
	}
}

func TestTheOwnerAcceptingAnAgentProposalIsWhatVerifiesIt(t *testing.T) {
	service := newStoredService(t)
	now := time.Now().UTC()
	fact, err := service.ProposeAgentFact(t.Context(), &AgentProposal{OwnerID: "owner-1", Category: "language", Key: "backend", Value: "Go"}, now)
	if err != nil {
		t.Fatalf("ProposeAgentFact: %v", err)
	}

	accepted, err := service.AcceptFact(t.Context(), "owner-1", fact.ID, now)

	if err != nil {
		t.Fatalf("AcceptFact: %v", err)
	}
	if !accepted.OwnerVerified || accepted.Status != StatusActive || accepted.Origin != OriginAgent {
		t.Fatalf("accepted = %+v, want an active verified fact that remembers its origin", accepted)
	}
	if served := servedFacts(t, service, now); len(served) != 1 || served[0].Origin != OriginAgent {
		t.Errorf("owner context serves %+v, want the accepted fact", served)
	}
}

func TestAnAgentProposalRepeatsWithoutChangeAndNeverRevivesADecidedValue(t *testing.T) {
	service := newStoredService(t)
	now := time.Now().UTC()
	proposal := &AgentProposal{OwnerID: "owner-1", Category: "habit", Key: "standup", Value: "09:30"}
	first, err := service.ProposeAgentFact(t.Context(), proposal, now)
	if err != nil {
		t.Fatalf("ProposeAgentFact: %v", err)
	}

	again, err := service.ProposeAgentFact(t.Context(), proposal, now.Add(time.Hour))

	if err != nil || again.ID != first.ID || !again.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("repeat = %+v, %v, want the recorded fact unchanged", again, err)
	}
	if err := service.RejectFact(t.Context(), "owner-1", first.ID, now); err != nil {
		t.Fatalf("RejectFact: %v", err)
	}
	if _, err := service.ProposeAgentFact(t.Context(), proposal, now.Add(2*time.Hour)); !fault.HasCode(err, ErrorCodeProfileConflict) {
		t.Errorf("proposing a rejected value = %v, want %s", err, ErrorCodeProfileConflict)
	}
}

func TestAnAgentProposalIsRefusedWhenItCannotBePersisted(t *testing.T) {
	service := newStoredService(t)
	now := time.Now().UTC()
	cases := []struct {
		name     string
		proposal *AgentProposal
		want     ErrorCode
	}{
		{name: "a sensitive trait", proposal: &AgentProposal{OwnerID: "owner-1", Category: "habit", Key: "religion", Value: "none"}, want: ErrorCodeProfileInvalid},
		{name: "an unknown category", proposal: &AgentProposal{OwnerID: "owner-1", Category: "mood", Key: "k", Value: "v"}, want: ErrorCodeInvalidArgument},
		{name: "an empty value", proposal: &AgentProposal{OwnerID: "owner-1", Category: "tool", Key: "k"}, want: ErrorCodeInvalidArgument},
		{name: "no owner", proposal: &AgentProposal{Category: "tool", Key: "k", Value: "v"}, want: ErrorCodeInvalidArgument},
		{name: "no proposal", want: ErrorCodeInvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := service.ProposeAgentFact(t.Context(), tc.proposal, now); !fault.HasCode(err, tc.want) {
				t.Fatalf("error = %v, want %s", err, tc.want)
			}
		})
	}
	if listed, err := service.ListFacts(t.Context(), "owner-1", "", "", 10); err != nil || len(listed) != 0 {
		t.Errorf("facts = %+v, %v, want none stored after the refusals", listed, err)
	}
}
