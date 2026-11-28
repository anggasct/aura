package memory

import (
	"context"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
)

const (
	maxInspectLimit = 200
	maxForgetLimit  = 200
	maxPurgeLimit   = 500
)

const redactedContent = "[redacted]"

func (s *Service) redactedCopy(document *StoredDocument) StoredDocument {
	if document == nil {
		return StoredDocument{}
	}
	copied := *document
	if s != nil && s.secrets != nil && s.secrets.Contains(copied.Content) {
		copied.Content = redactedContent
	}
	return copied
}

func (s *Service) PreviewForgetBySession(ctx context.Context, ownerID, sessionID string, limit int) ([]StoredDocument, error) {
	if ctx == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(sessionID) == "" {
		return nil, fault.New(ErrorCodeInvalidArgument, "owner and session must not be empty")
	}
	if limit <= 0 {
		limit = maxForgetLimit
	}
	if limit > maxForgetLimit {
		return nil, fault.New(ErrorCodeInvalidArgument, "forget limit is out of bounds")
	}
	owned, err := s.documents.ListSessionDocuments(ctx, ownerID, sessionID, limit)
	if err != nil {
		return nil, err
	}
	if len(owned) > 0 {
		preview := make([]StoredDocument, 0, len(owned))
		for i := range owned {
			preview = append(preview, s.redactedCopy(&owned[i]))
		}
		return preview, nil
	}
	total, err := s.documents.CountSessionDocuments(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if total > 0 {
		return nil, fault.New(ErrorCodeForbidden, "memory session is owned by another owner")
	}
	return []StoredDocument{}, nil
}

func (s *Service) PreviewForgetByID(ctx context.Context, ownerID, documentID string) ([]StoredDocument, error) {
	if ctx == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(documentID) == "" {
		return nil, fault.New(ErrorCodeInvalidArgument, "owner and document id must not be empty")
	}
	document, found, err := s.documents.GetDocument(ctx, documentID)
	if err != nil {
		return nil, err
	}
	if !found {
		return []StoredDocument{}, nil
	}
	if document.OwnerID != ownerID {
		return nil, fault.New(ErrorCodeForbidden, "memory document is owned by another owner")
	}
	return []StoredDocument{s.redactedCopy(&document)}, nil
}

func (s *Service) PreviewForgetByQuery(ctx context.Context, ownerID, sessionID, query string, limit int) ([]StoredDocument, error) {
	if ctx == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(query) == "" {
		return nil, fault.New(ErrorCodeInvalidArgument, "owner and query must not be empty")
	}
	if limit <= 0 {
		limit = maxForgetLimit
	}
	if limit > maxForgetLimit {
		return nil, fault.New(ErrorCodeInvalidArgument, "forget limit is out of bounds")
	}
	hits, err := s.documents.Search(ctx, &StoredQuery{
		OwnerID:   ownerID,
		SessionID: sessionID,
		Terms:     query,
		Limit:     limit,
	})
	if err != nil {
		return nil, err
	}
	preview := make([]StoredDocument, 0, len(hits))
	for i := range hits {
		preview = append(preview, s.redactedCopy(&hits[i].StoredDocument))
	}
	return preview, nil
}

func (s *Service) Forget(ctx context.Context, ownerID string, documents []StoredDocument) (int, error) {
	if ctx == nil {
		return 0, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if len(documents) == 0 {
		return 0, nil
	}
	ids := make([]string, 0, len(documents))
	for i := range documents {
		ids = append(ids, documents[i].ID)
	}
	return s.documents.DeleteDocumentsByIDsOwned(ctx, ownerID, ids)
}

func (s *Service) PreviewPurge(ctx context.Context, now time.Time, limit int) ([]StoredDocument, error) {
	if ctx == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if limit <= 0 || limit > maxPurgeLimit {
		return nil, fault.New(ErrorCodeInvalidArgument, "purge limit is out of bounds")
	}
	expired, err := s.documents.ListExpiredSummaries(ctx, now.UTC(), limit)
	if err != nil {
		return nil, err
	}
	preview := make([]StoredDocument, 0, len(expired))
	for i := range expired {
		preview = append(preview, s.redactedCopy(&expired[i]))
	}
	return preview, nil
}

func (s *Service) Inspect(ctx context.Context, ownerID, sessionID, query string, limit int) ([]StoredDocument, error) {
	if ctx == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(ownerID) == "" {
		return nil, fault.New(ErrorCodeInvalidArgument, "owner must not be empty")
	}
	if limit <= 0 {
		limit = maxInspectLimit
	}
	if limit > maxInspectLimit {
		return nil, fault.New(ErrorCodeInvalidArgument, "inspect limit is out of bounds")
	}
	if strings.TrimSpace(query) != "" {
		hits, err := s.documents.Search(ctx, &StoredQuery{
			OwnerID:   ownerID,
			SessionID: sessionID,
			Terms:     query,
			Limit:     limit,
		})
		if err != nil {
			return nil, err
		}
		documents := make([]StoredDocument, 0, len(hits))
		for i := range hits {
			documents = append(documents, s.redactedCopy(&hits[i].StoredDocument))
		}
		return documents, nil
	}
	var documents []StoredDocument
	var err error
	if strings.TrimSpace(sessionID) != "" {
		documents, err = s.documents.ListSessionDocuments(ctx, ownerID, sessionID, limit)
	} else {
		documents, err = s.documents.ListOwnerDocuments(ctx, ownerID, limit)
	}
	if err != nil {
		return nil, err
	}
	out := make([]StoredDocument, 0, len(documents))
	for i := range documents {
		out = append(out, s.redactedCopy(&documents[i]))
	}
	return out, nil
}
