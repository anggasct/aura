package memory

import (
	"context"
	"strings"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

const runOutputSeparator = "\n\n"

func (s *Service) ProjectRunOutput(ctx context.Context, ownerID string, events []*Event) (BatchResult, error) {
	var result BatchResult
	if ctx == nil {
		return result, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if strings.TrimSpace(ownerID) == "" {
		return result, fault.New(ErrorCodeInvalidArgument, "owner must not be empty")
	}
	if s == nil || s.documents == nil {
		return result, fault.New(ErrorCodeInvalidArgument, "memory service must not be nil")
	}
	output, found := runOutputOf(events)
	if !found {
		result.Skipped = len(events)
		return result, nil
	}
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	droppedBefore := s.secretDrops.Load()
	_, indexed, err := s.writeProjection(ctx, ownerID, output)
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		s.projectionFailures.Add(1)
		return result, fault.New(ErrorCodeProjectionStale, "projection store unavailable")
	}
	result.Dropped = int(s.secretDrops.Load() - droppedBefore)
	if indexed {
		result.Indexed = 1
	}
	result.Skipped = max(len(events)-result.Indexed-result.Dropped, 0)
	return result, nil
}

func runOutputOf(events []*Event) (*projection, bool) {
	var texts []string
	var first, last *Event
	for _, event := range events {
		if event == nil || event.Kind != turnstore.EventKindADK || event.Author == "user" {
			continue
		}
		if turnstore.HasToolActivity(event.Payload) {
			continue
		}
		text := turnstore.ModelText(event.Payload, false)
		if strings.TrimSpace(text) == "" {
			continue
		}
		texts = append(texts, text)
		if first == nil {
			first = event
		}
		last = event
	}
	if first == nil {
		return nil, false
	}
	return &projection{
		sessionID: first.SessionID, from: first.Sequence, to: last.Sequence,
		text: strings.Join(texts, runOutputSeparator), trust: approval.TrustDerivedUntrusted, createdAt: last.CreatedAt,
	}, true
}
