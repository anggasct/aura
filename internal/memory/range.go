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
	s.projectMu.Lock()
	probe, err := s.documents.ListSessionDocumentsByKind(ctx, ownerID, sessionID, KindEventText, s.rangeThreshold+1)
	if err != nil {
		s.projectMu.Unlock()
		return RecallContext{}, false, err
	}
	if len(probe) < s.rangeThreshold {
		s.projectMu.Unlock()
		return RecallContext{}, false, nil
	}
	mark, found, err := s.documents.ProjectionWatermark(ctx, sessionID)
	if err != nil {
		s.projectMu.Unlock()
		return RecallContext{}, false, err
	}
	if !found {
		s.projectMu.Unlock()
		return RecallContext{}, false, nil
	}
	minRaw, err := sequenceFromDB(probe[0].FromSequence)
	if err != nil {
		s.projectMu.Unlock()
		return RecallContext{}, false, Errorf(ErrorCodeProjectionCorrupt, "projected document carries an invalid sequence")
	}
	closedTo := mark - int64(s.rangeLiveWindow)
	minDB, err := sequenceToDB(minRaw)
	if err != nil {
		s.projectMu.Unlock()
		return RecallContext{}, false, err
	}
	if closedTo < minDB {
		s.projectMu.Unlock()
		return RecallContext{}, false, nil
	}
	candidates, err := s.documents.ListSessionDocumentsByKind(ctx, ownerID, sessionID, KindEventText, maxRangeFetch)
	if err != nil {
		s.projectMu.Unlock()
		return RecallContext{}, false, err
	}
	filtered := make([]RecallDocument, 0, len(candidates))
	for i := range candidates {
		if candidates[i].FromSequence < minDB || candidates[i].ToSequence > closedTo {
			continue
		}
		document, err := storedToRecall(&candidates[i])
		if err != nil {
			s.projectMu.Unlock()
			return RecallContext{}, false, err
		}
		filtered = append(filtered, document)
	}
	if len(filtered) == 0 {
		s.projectMu.Unlock()
		return RecallContext{}, false, nil
	}
	from, to := filtered[0].FromSequence, filtered[0].ToSequence
	for i := range filtered[1:] {
		if filtered[1+i].FromSequence < from {
			from = filtered[1+i].FromSequence
		}
		if filtered[1+i].ToSequence > to {
			to = filtered[1+i].ToSequence
		}
	}
	existing, err := s.documents.ListSessionDocumentsByKind(ctx, ownerID, sessionID, KindSummary, 10)
	if err != nil {
		s.projectMu.Unlock()
		return RecallContext{}, false, err
	}
	for i := range existing {
		existingFrom, err := sequenceFromDB(existing[i].FromSequence)
		if err != nil {
			s.projectMu.Unlock()
			return RecallContext{}, false, err
		}
		existingTo, err := sequenceFromDB(existing[i].ToSequence)
		if err != nil {
			s.projectMu.Unlock()
			return RecallContext{}, false, err
		}
		if existingFrom == from && existingTo <= to && existing[i].PromptVersion == s.summaryPrompt {
			if _, err := s.documents.DeleteOtherSummaries(ctx, ownerID, sessionID, existing[i].ID); err != nil {
				s.projectMu.Unlock()
				return RecallContext{}, false, err
			}
			s.projectMu.Unlock()
			return RecallContext{}, false, nil
		}
	}
	s.projectMu.Unlock()
	result, err := s.Summarize(ctx, ownerID, sessionID, "", filtered, model, summarizer)
	if err != nil {
		if code, ok := CodeOf(err); ok && code == ErrorCodeModelCapabilityUnsupported {
			s.observe(ctx, &Observation{
				Outcome:       string(ErrorCodeModelCapabilityUnsupported),
				Documents:     len(filtered),
				ModelProtocol: model.Protocol,
				ModelName:     model.Name,
			})
			return RecallContext{}, false, nil
		}
		return RecallContext{}, false, err
	}
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	keepID := documentID(sessionID, KindSummary, from, to, s.summaryPrompt)
	summaries, err := s.documents.ListSessionDocumentsByKind(ctx, ownerID, sessionID, KindSummary, 10)
	if err != nil {
		return result, true, err
	}
	present := false
	for i := range summaries {
		if summaries[i].ID == keepID {
			present = true
			break
		}
	}
	if !present {
		return RecallContext{}, false, nil
	}
	if _, err := s.documents.DeleteOtherSummaries(ctx, ownerID, sessionID, keepID); err != nil {
		return result, true, err
	}
	return result, true, nil
}
