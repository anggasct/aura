package profile

import (
	stdcontext "context"
	"strings"
	"time"
	"unicode"

	"github.com/anggasct/aura/internal/platform/fault"
)

type SearchHit struct {
	Fact
	Rank float64
}

type RetrieveQuery struct {
	OwnerID      string
	Category     string
	Query        string
	MaxFacts     int
	MaxTokens    int
	VerifiedOnly bool
	StatedKeys   []string
}

func NormalizeKey(key string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(key), func(r rune) bool {
		return r == '_' || r == '-' || unicode.IsSpace(r)
	}), " ")
}

func estimateTokens(text string) int {
	if tokens := len(text) / 4; tokens > 1 {
		return tokens
	}
	return 1
}

func (s *Service) Retrieve(ctx stdcontext.Context, query *RetrieveQuery, now time.Time) (*ContextPart, error) {
	if ctx == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if query == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "query must not be nil")
	}
	if strings.TrimSpace(query.OwnerID) == "" {
		return nil, fault.New(ErrorCodeInvalidArgument, "owner must not be empty")
	}
	if query.MaxFacts <= 0 || query.MaxTokens <= 0 {
		return nil, fault.New(ErrorCodeInvalidArgument, "retrieval budgets must be positive")
	}
	if now.IsZero() {
		return nil, fault.New(ErrorCodeInvalidArgument, "timestamp must not be zero")
	}
	hits, err := s.registry.Search(ctx, query.OwnerID, query.Category, strings.TrimSpace(query.Query), query.VerifiedOnly, query.MaxFacts*4, now)
	if err != nil {
		return nil, err
	}
	stated := make(map[string]bool, len(query.StatedKeys))
	for _, key := range query.StatedKeys {
		if normalized := NormalizeKey(key); normalized != "" {
			stated[normalized] = true
		}
	}
	part := &ContextPart{OwnerID: query.OwnerID}
	used, overridden := 0, 0
	for i := range hits {
		hit := &hits[i]
		if hit.Status != StatusActive || hit.OwnerID != query.OwnerID {
			continue
		}
		if query.VerifiedOnly && !hit.OwnerVerified {
			continue
		}
		if hit.ExpiresAt != nil && !hit.ExpiresAt.After(now) {
			continue
		}
		if s.scanner.Contains(hit.Key) || s.scanner.Contains(hit.Value) {
			continue
		}
		if stated[NormalizeKey(hit.Key)] {
			overridden++
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
	observeWith(ctx, s.observer, &Observation{Kind: ObserveContext, Facts: len(part.Facts), Tokens: used, Overridden: overridden})
	return part, nil
}
