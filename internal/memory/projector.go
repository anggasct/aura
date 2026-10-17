package memory

import (
	"context"
	"strings"
)

type BatchResult struct {
	Indexed int
	Dropped int
	Skipped int
}

func (s *Service) ProjectBatch(ctx context.Context, ownerID string, events []*Event) (BatchResult, error) {
	var result BatchResult
	if ctx == nil {
		return result, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if strings.TrimSpace(ownerID) == "" {
		return result, Errorf(ErrorCodeInvalidArgument, "owner must not be empty")
	}
	if s == nil {
		return result, Errorf(ErrorCodeInvalidArgument, "memory service must not be nil")
	}
	if s.documents == nil {
		return result, Errorf(ErrorCodeInvalidArgument, "document store must not be nil")
	}
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	droppedBefore := s.secretDrops.Load()
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		_, indexed, err := s.ProjectEvent(ctx, ownerID, event)
		if err != nil {
			if code, ok := CodeOf(err); ok && code == ErrorCodeInvalidArgument {
				result.Skipped++
				continue
			}
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			s.projectionFailures.Add(1)
			return result, Errorf(ErrorCodeProjectionStale, "projection store unavailable")
		}
		if indexed {
			result.Indexed++
			continue
		}
		result.Skipped++
	}
	result.Dropped = int(s.secretDrops.Load() - droppedBefore)
	result.Skipped -= result.Dropped
	if result.Skipped < 0 {
		result.Skipped = 0
	}
	return result, nil
}
