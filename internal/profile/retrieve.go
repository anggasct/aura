package profile

import (
	"strings"
	"time"

	stdcontext "context"
)

type SearchHit struct {
	Fact
	Rank float64
}

type RetrieveQuery struct {
	OwnerID   string
	Category  string
	Query     string
	MaxFacts  int
	MaxTokens int
}

func estimateTokens(text string) int {
	if tokens := len(text) / 4; tokens > 1 {
		return tokens
	}
	return 1
}

func (s *Service) Retrieve(ctx stdcontext.Context, query *RetrieveQuery, now time.Time) (*ContextPart, error) {
	if ctx == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if query == nil {
		return nil, errNilArgument("query")
	}
	if strings.TrimSpace(query.OwnerID) == "" {
		return nil, Errorf(ErrorCodeInvalidArgument, "owner must not be empty")
	}
	if query.MaxFacts <= 0 || query.MaxTokens <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "retrieval budgets must be positive")
	}
	if now.IsZero() {
		return nil, Errorf(ErrorCodeInvalidArgument, "timestamp must not be zero")
	}
	hits, err := s.registry.Search(ctx, query.OwnerID, query.Category, strings.TrimSpace(query.Query), query.MaxFacts*4)
	if err != nil {
		return nil, err
	}
	part := &ContextPart{OwnerID: query.OwnerID}
	used := 0
	for i := range hits {
		hit := &hits[i]
		if hit.Status != StatusActive || hit.OwnerID != query.OwnerID {
			continue
		}
		if hit.ExpiresAt != nil && !hit.ExpiresAt.After(now) {
			continue
		}
		if len(part.Facts) >= query.MaxFacts {
			break
		}
		cost := estimateTokens(hit.Key + " " + hit.Value)
		if used+cost > query.MaxTokens {
			continue
		}
		used += cost
		part.Facts = append(part.Facts, ContextFact{
			ID: hit.ID, Category: hit.Category, Key: hit.Key, Value: hit.Value,
			Confidence: hit.Confidence, Origin: hit.Origin, Verified: hit.OwnerVerified,
		})
	}
	return part, nil
}
