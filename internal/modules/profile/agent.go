package profile

import (
	stdcontext "context"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
)

type AgentProposal struct {
	OwnerID   string
	Category  string
	Key       string
	Value     string
	ExpiresAt *time.Time
}

func (s *Service) ProposeAgentFact(ctx stdcontext.Context, proposal *AgentProposal, now time.Time) (Fact, error) {
	if ctx == nil {
		return Fact{}, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return Fact{}, err
	}
	if proposal == nil {
		return Fact{}, fault.New(ErrorCodeInvalidArgument, "agent proposal must not be nil")
	}
	if err := checkProposal(proposal.OwnerID, proposal.Category, proposal.Key, proposal.Value); err != nil {
		return Fact{}, err
	}
	if reason := SensitiveReason(proposal.Category, proposal.Key, proposal.Value, s.scanner); reason != "" {
		return Fact{}, fault.New(ErrorCodeProfileInvalid, "fact is not persistable")
	}
	if now.IsZero() {
		return Fact{}, fault.New(ErrorCodeInvalidArgument, "timestamp must not be zero")
	}
	digest := ValueDigest(proposal.Value)
	id := FactID(proposal.OwnerID, proposal.Category, proposal.Key, digest)
	existing, found, err := s.registry.Get(ctx, id)
	if err != nil {
		return Fact{}, err
	}
	if found {
		return keptForReview(&existing)
	}
	actives, err := s.registry.Active(ctx, proposal.OwnerID, proposal.Category, proposal.Key)
	if err != nil {
		return Fact{}, err
	}
	fact := Fact{
		ID: id, OwnerID: proposal.OwnerID, Category: proposal.Category, Key: proposal.Key, Value: proposal.Value,
		ValueDigest: digest, Status: StatusCandidate, Origin: OriginAgent, OwnerVerified: false,
		Confidence: ConfidenceV1(0), ConfidencePolicy: ConfidencePolicyV1,
		ExpiresAt: proposal.ExpiresAt, CreatedAt: now, UpdatedAt: now,
	}
	for i := range actives {
		if actives[i].ValueDigest != digest {
			fact.ConflictsWithID = actives[i].ID
		}
	}
	return s.registry.InsertCandidate(ctx, &fact, nil, 0)
}

func keptForReview(existing *Fact) (Fact, error) {
	switch existing.Status {
	case StatusCandidate, StatusActive:
		return *existing, nil
	case StatusRejected, StatusExpired, StatusDeleted:
		return Fact{}, fault.New(ErrorCodeProfileConflict, "a decided value cannot be proposed again")
	}
	return Fact{}, fault.New(ErrorCodeProfileConflict, "fact is in a terminal state")
}
