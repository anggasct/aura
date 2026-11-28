package memory

import (
	"context"
	"strings"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

const runOutputSeparator = "\n\n"

func (s *Service) ProjectRunOutput(ctx context.Context, ownerID string, events []*Event) (BatchResult, error) {
	return s.projectRunOutput(ctx, ownerID, nil, events)
}

func (s *Service) ProjectRunOutputForTurn(ctx context.Context, req *turn.TurnRequest, events []*Event) (BatchResult, error) {
	if req == nil {
		return BatchResult{}, fault.New(ErrorCodeInvalidArgument, "turn request must not be nil")
	}
	return s.projectRunOutput(ctx, req.PrincipalID, req, events)
}

func (s *Service) projectRunOutput(ctx context.Context, ownerID string, req *turn.TurnRequest, events []*Event) (BatchResult, error) {
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
	taintCtx := s.buildBatchTaintContext(ctx, req, events)
	output.tainted = taintCtx.isOtherEventTainted(ownerID, output.sessionID, output.to)
	var embeddedDoc Document
	var shouldEmbed bool
	err := func() error {
		s.projectMu.Lock()
		defer s.projectMu.Unlock()
		droppedBefore := s.secretDrops.Load()
		doc, indexed, inserted, err := s.writeProjection(ctx, ownerID, output)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.projectionFailures.Add(1)
			return fault.New(ErrorCodeProjectionStale, "projection store unavailable")
		}
		result.Dropped = int(s.secretDrops.Load() - droppedBefore)
		if indexed {
			result.Indexed = 1
		}
		if inserted {
			embeddedDoc = doc
			shouldEmbed = true
		}
		result.Skipped = max(len(events)-result.Indexed-result.Dropped, 0)
		return nil
	}()
	if err != nil {
		return result, err
	}
	if shouldEmbed {
		s.embedDocumentBestEffort(ctx, embeddedDoc.ID, embeddedDoc.OwnerID, embeddedDoc.Content)
	}
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
