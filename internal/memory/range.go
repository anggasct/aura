package memory

import (
	"context"
	"strings"

	"github.com/anggasct/aura/internal/approval"
)

func storedToRecall(document *StoredDocument) (RecallDocument, error) {
	from, err := sequenceFromDB(document.FromSequence)
	if err != nil {
		return RecallDocument{}, Errorf(ErrorCodeProjectionCorrupt, "projected document carries an invalid sequence")
	}
	to, err := sequenceFromDB(document.ToSequence)
	if err != nil {
		return RecallDocument{}, Errorf(ErrorCodeProjectionCorrupt, "projected document carries an invalid sequence")
	}
	return RecallDocument{
		ID:            document.ID,
		SessionID:     document.SessionID,
		FromSequence:  from,
		ToSequence:    to,
		Content:       document.Content,
		Trust:         approval.TrustLabel(document.TrustLabel),
		PromptVersion: document.PromptVersion,
		ModelProtocol: document.ModelProtocol,
		ModelName:     document.ModelName,
		CreatedAt:     document.CreatedAt,
		ExpiresAt:     document.ExpiresAt,
	}, nil
}

const (
	maxRangeSummaries     = 500
	maxRangeChunksPerPass = 4
)

func (s *Service) SummarizeClosedRange(ctx context.Context, ownerID, sessionID string, model *SummaryModel, summarizer Summarizer) (RecallContext, bool, error) {
	if ctx == nil {
		return RecallContext{}, false, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return RecallContext{}, false, err
	}
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(sessionID) == "" {
		return RecallContext{}, false, Errorf(ErrorCodeInvalidArgument, "owner and session must not be empty")
	}
	if model == nil {
		return RecallContext{}, false, Errorf(ErrorCodeInvalidArgument, "summary model must not be nil")
	}
	if summarizer == nil {
		return RecallContext{}, false, Errorf(ErrorCodeInvalidArgument, "summarizer must not be nil")
	}
	if s == nil {
		return RecallContext{}, false, Errorf(ErrorCodeInvalidArgument, "memory service must not be nil")
	}
	if s.documents == nil {
		return RecallContext{}, false, Errorf(ErrorCodeInvalidArgument, "document store must not be nil")
	}
	var result RecallContext
	summarized := false
	for chunkCount := 0; chunkCount < maxRangeChunksPerPass; chunkCount++ {
		chunk, ok, err := s.nextClosedRangeChunk(ctx, ownerID, sessionID)
		if err != nil {
			return RecallContext{}, summarized, err
		}
		if !ok {
			return result, summarized, nil
		}
		from, to := chunk[0].FromSequence, chunk[0].ToSequence
		for i := range chunk[1:] {
			if chunk[1+i].FromSequence < from {
				from = chunk[1+i].FromSequence
			}
			if chunk[1+i].ToSequence > to {
				to = chunk[1+i].ToSequence
			}
		}
		next, err := s.Summarize(ctx, ownerID, sessionID, "", chunk, model, summarizer)
		if err != nil {
			if code, ok := CodeOf(err); ok && code == ErrorCodeModelCapabilityUnsupported {
				s.observe(ctx, &Observation{
					Outcome:       string(ErrorCodeModelCapabilityUnsupported),
					Documents:     len(chunk),
					ModelProtocol: model.Protocol,
					ModelName:     model.Name,
				})
				return RecallContext{}, summarized, nil
			}
			return RecallContext{}, summarized, err
		}
		keepID := documentID(sessionID, KindSummary, from, to, s.summaryPrompt)
		present, err := s.summaryPresent(ctx, ownerID, sessionID, keepID)
		if err != nil {
			return next, summarized, err
		}
		if !present {
			return result, summarized, nil
		}
		result = next
		summarized = true
	}
	return result, summarized, nil
}

func (s *Service) nextClosedRangeChunk(ctx context.Context, ownerID, sessionID string) ([]RecallDocument, bool, error) {
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	probe, err := s.documents.ListSessionDocumentsByKind(ctx, ownerID, sessionID, KindEventText, s.rangeThreshold+1)
	if err != nil {
		return nil, false, err
	}
	if len(probe) < s.rangeThreshold {
		return nil, false, nil
	}
	mark, found, err := s.documents.ProjectionWatermark(ctx, sessionID)
	if err != nil || !found {
		return nil, false, err
	}
	minRaw, err := sequenceFromDB(probe[0].FromSequence)
	if err != nil {
		return nil, false, Errorf(ErrorCodeProjectionCorrupt, "projected document carries an invalid sequence")
	}
	minDB, err := sequenceToDB(minRaw)
	if err != nil {
		return nil, false, err
	}
	closedTo := mark - int64(s.rangeLiveWindow)
	if closedTo < minDB {
		return nil, false, nil
	}
	coveredTo, err := s.coveredRange(ctx, ownerID, sessionID, minRaw)
	if err != nil {
		return nil, false, err
	}
	fromRaw := coveredTo + 1
	if fromRaw < minRaw {
		fromRaw = minRaw
	}
	fromDB, err := sequenceToDB(fromRaw)
	if err != nil {
		return nil, false, err
	}
	if closedTo-fromDB+1 < s.rangeSummaryGrowth() {
		return nil, false, nil
	}
	candidates, err := s.documents.ListSessionDocumentsByKindInRange(ctx, ownerID, sessionID, KindEventText, fromDB, closedTo, maxRangeFetch)
	if err != nil {
		return nil, false, err
	}
	filtered := make([]RecallDocument, 0, len(candidates))
	for i := range candidates {
		document, err := storedToRecall(&candidates[i])
		if err != nil {
			return nil, false, err
		}
		filtered = append(filtered, document)
	}
	if len(filtered) == 0 {
		return nil, false, nil
	}
	return filtered, true, nil
}

func (s *Service) coveredRange(ctx context.Context, ownerID, sessionID string, minRaw uint64) (uint64, error) {
	summaries, err := s.documents.ListSessionDocumentsByKind(ctx, ownerID, sessionID, KindSummary, maxRangeSummaries)
	if err != nil {
		return 0, err
	}
	covered := minRaw - 1
	anchored := false
	for i := range summaries {
		if summaries[i].PromptVersion != s.summaryPrompt {
			continue
		}
		from, err := sequenceFromDB(summaries[i].FromSequence)
		if err != nil {
			return 0, Errorf(ErrorCodeProjectionCorrupt, "projected document carries an invalid sequence")
		}
		to, err := sequenceFromDB(summaries[i].ToSequence)
		if err != nil {
			return 0, Errorf(ErrorCodeProjectionCorrupt, "projected document carries an invalid sequence")
		}
		if from == minRaw {
			anchored = true
		}
		if to > covered {
			covered = to
		}
	}
	if !anchored {
		return minRaw - 1, nil
	}
	return covered, nil
}

func (s *Service) rangeSummaryGrowth() int64 {
	step := int64(s.rangeLiveWindow)
	if step < 1 {
		step = 1
	}
	return step
}

func (s *Service) summaryPresent(ctx context.Context, ownerID, sessionID, keepID string) (bool, error) {
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	summaries, err := s.documents.ListSessionDocumentsByKind(ctx, ownerID, sessionID, KindSummary, maxRangeSummaries)
	if err != nil {
		return false, err
	}
	for i := range summaries {
		if summaries[i].ID == keepID {
			return true, nil
		}
	}
	return false, nil
}
