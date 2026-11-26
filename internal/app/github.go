package app

import (
	"context"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/fault"
)

func githubConfig(cfg *appconfig.Config) *github.Config {
	if cfg == nil || cfg.Tools == nil {
		return nil
	}
	return &cfg.Tools.GitHub
}

type workflowCorrelationStore struct {
	store *workflow.Store
}

func newWorkflowCorrelationStore(store *workflow.Store) *workflowCorrelationStore {
	return &workflowCorrelationStore{store: store}
}

func (s *workflowCorrelationStore) BindCorrelation(ctx context.Context, correlation *github.Correlation) error {
	if correlation == nil {
		return fault.New(github.ErrorCodeInvalidArgument, "correlation must not be nil")
	}
	err := s.store.BindCorrelation(ctx, &workflow.Correlation{
		Source:     correlation.Source,
		EventType:  correlation.EventType,
		ExternalID: correlation.ExternalID,
		RunID:      correlation.RunID,
		SignalName: correlation.SignalName,
		DedupeKey:  correlation.DedupeKey,
	})
	return mapCorrelationError(err)
}

func (s *workflowCorrelationStore) ResolveCorrelation(ctx context.Context, source, eventType, externalID, dedupeKey string) (github.Correlation, error) {
	found, err := s.store.ResolveCorrelation(ctx, source, eventType, externalID, dedupeKey)
	if err != nil {
		return github.Correlation{}, mapCorrelationError(err)
	}
	return github.Correlation{
		Source:     found.Source,
		EventType:  found.EventType,
		ExternalID: found.ExternalID,
		RunID:      found.RunID,
		SignalName: found.SignalName,
		DedupeKey:  found.DedupeKey,
	}, nil
}

func (s *workflowCorrelationStore) DeleteCorrelation(ctx context.Context, source, eventType, externalID, dedupeKey string) error {
	return mapCorrelationError(s.store.DeleteCorrelation(ctx, source, eventType, externalID, dedupeKey))
}

func (s *workflowCorrelationStore) RunDurableKey(ctx context.Context, runID string) (string, error) {
	summary, err := s.store.Run(ctx, runID)
	if err != nil {
		return "", err
	}
	return summary.DurableKey, nil
}

func mapCorrelationError(err error) error {
	if err == nil {
		return nil
	}
	if code, ok := fault.CodeOf[workflow.ErrorCode](err); ok {
		if code == workflow.ErrorCodeCorrelationUnmatched {
			return fault.Wrap(github.ErrorCodeCorrelationUnmatched, err.Error(), err)
		}
		if code == workflow.ErrorCodeCorrelationConflict {
			return fault.Wrap(github.ErrorCodeCorrelationConflict, err.Error(), err)
		}
	}
	return err
}
