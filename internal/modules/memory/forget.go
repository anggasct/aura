package memory

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
)

const (
	maxListLimit    = 200
	maxInspectLimit = maxListLimit
	maxForgetLimit  = maxListLimit
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

type DocumentQuery struct {
	OwnerID    string
	AgentIDs   []string
	SessionID  string
	DocumentID string
	Terms      string
	Limit      int
}

func (s *Service) checkQuery(ctx context.Context, query *DocumentQuery) (int, error) {
	if ctx == nil {
		return 0, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if query == nil {
		return 0, fault.New(ErrorCodeInvalidArgument, "document query must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if strings.TrimSpace(query.OwnerID) == "" {
		return 0, fault.New(ErrorCodeInvalidArgument, "owner must not be empty")
	}
	limit := query.Limit
	if limit <= 0 {
		limit = maxListLimit
	}
	if limit > maxListLimit {
		return 0, fault.New(ErrorCodeInvalidArgument, "document limit is out of bounds")
	}
	return limit, nil
}

func (s *Service) redactedCopies(documents []StoredDocument) []StoredDocument {
	copies := make([]StoredDocument, 0, len(documents))
	for i := range documents {
		copies = append(copies, s.redactedCopy(&documents[i]))
	}
	return copies
}

func (s *Service) PreviewForgetBySession(ctx context.Context, query *DocumentQuery) ([]StoredDocument, error) {
	limit, err := s.checkQuery(ctx, query)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(query.SessionID) == "" {
		return nil, fault.New(ErrorCodeInvalidArgument, "owner and session must not be empty")
	}
	owned, err := s.documents.ListDocuments(ctx, &DocumentFilter{
		OwnerID: query.OwnerID, SessionID: query.SessionID, AgentIDs: query.AgentIDs, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	if len(owned) > 0 {
		return s.redactedCopies(owned), nil
	}
	total, err := s.documents.CountSessionDocuments(ctx, query.SessionID)
	if err != nil {
		return nil, err
	}
	if total > 0 {
		return nil, fault.New(ErrorCodeForbidden, "memory session is owned by another owner or persona")
	}
	return []StoredDocument{}, nil
}

func (s *Service) PreviewForgetByID(ctx context.Context, query *DocumentQuery) ([]StoredDocument, error) {
	if _, err := s.checkQuery(ctx, query); err != nil {
		return nil, err
	}
	if strings.TrimSpace(query.DocumentID) == "" {
		return nil, fault.New(ErrorCodeInvalidArgument, "owner and document id must not be empty")
	}
	document, found, err := s.documents.GetDocument(ctx, query.DocumentID)
	if err != nil {
		return nil, err
	}
	if !found {
		return []StoredDocument{}, nil
	}
	if document.OwnerID != query.OwnerID {
		return nil, fault.New(ErrorCodeForbidden, "memory document is owned by another owner")
	}
	if len(query.AgentIDs) > 0 && !slices.Contains(query.AgentIDs, document.AgentID) {
		return []StoredDocument{}, nil
	}
	return []StoredDocument{s.redactedCopy(&document)}, nil
}

func (s *Service) PreviewForgetByQuery(ctx context.Context, query *DocumentQuery) ([]StoredDocument, error) {
	limit, err := s.checkQuery(ctx, query)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(query.Terms) == "" {
		return nil, fault.New(ErrorCodeInvalidArgument, "owner and query must not be empty")
	}
	hits, err := s.documents.Search(ctx, &StoredQuery{
		OwnerID: query.OwnerID, AgentIDs: query.AgentIDs, SessionID: query.SessionID, Terms: query.Terms, Limit: limit,
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

func (s *Service) Inspect(ctx context.Context, query *DocumentQuery) ([]StoredDocument, error) {
	limit, err := s.checkQuery(ctx, query)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(query.Terms) != "" {
		hits, err := s.documents.Search(ctx, &StoredQuery{
			OwnerID: query.OwnerID, AgentIDs: query.AgentIDs, SessionID: query.SessionID, Terms: query.Terms, Limit: limit,
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
	documents, err := s.documents.ListDocuments(ctx, &DocumentFilter{
		OwnerID: query.OwnerID, SessionID: strings.TrimSpace(query.SessionID), AgentIDs: query.AgentIDs, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	return s.redactedCopies(documents), nil
}
