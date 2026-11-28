package knowledge

import (
	"context"
	"errors"
	"fmt"

	"github.com/anggasct/aura/internal/platform/sqlite"
)

func (f *serviceFixture) prepared(ctx context.Context, collection, source string) (*Prepared, error) {
	if IsURLReference(source) {
		return f.service.PrepareURL(ctx, collection, source)
	}
	return f.service.PrepareHostFile(ctx, collection, source)
}

func (f *serviceFixture) add(ctx context.Context, collection, source string) (IngestResult, error) {
	prepared, err := f.prepared(ctx, collection, source)
	if err != nil {
		return IngestResult{}, err
	}
	result, err := f.service.Write(ctx, ActionAdd, prepared)
	if err != nil || result.Status == StatusUnchanged {
		return result, err
	}
	outcome, embedErr := f.service.EmbedAfterWrite(ctx, collection)
	result.Embedding = outcome
	return result, embedErr
}

func (f *serviceFixture) refresh(ctx context.Context, collection, source string) ([]IngestResult, error) {
	records, err := f.service.Sources(ctx, collection, source)
	if err != nil {
		return nil, err
	}
	var results []IngestResult
	var failures []error
	for i := range records {
		prepared, err := f.prepared(ctx, collection, records[i].URI)
		if err != nil {
			failures = append(failures, fmt.Errorf("refresh %s: %w", records[i].Label, err))
			continue
		}
		result, err := f.service.Write(ctx, ActionRefresh, prepared)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		results = append(results, result)
	}
	if !slicesChanged(results) {
		return results, errors.Join(failures...)
	}
	outcome, embedErr := f.service.EmbedAfterWrite(ctx, collection)
	for i := range results {
		results[i].Embedding = outcome
	}
	return results, errors.Join(append(failures, embedErr)...)
}

func (f *serviceFixture) embedOff(ctx context.Context, collection string, apply bool) (DisableOutcome, error) {
	if !apply {
		return f.service.PreviewEmbedOff(ctx, collection)
	}
	return f.service.EmbedOff(ctx, collection)
}

func (s *SQLiteStore) removeAndTruncate(ctx context.Context, collection, uri string) (RemoveOutcome, bool, error) {
	var outcome RemoveOutcome
	err := sqlite.RunInTx(ctx, s.db, func(txCtx context.Context) error {
		var err error
		outcome, err = s.DeleteSource(txCtx, collection, uri)
		return err
	})
	if err != nil {
		return RemoveOutcome{}, false, err
	}
	truncated, err := s.TruncateWAL(ctx)
	return outcome, truncated, err
}

func (s *SQLiteStore) disableAndTruncate(ctx context.Context, collection string) (DisableOutcome, bool, error) {
	var outcome DisableOutcome
	err := sqlite.RunInTx(ctx, s.db, func(txCtx context.Context) error {
		var err error
		outcome, err = s.DisableEmbedding(txCtx, collection)
		return err
	})
	if err != nil {
		return DisableOutcome{}, false, err
	}
	truncated, err := s.TruncateWAL(ctx)
	return outcome, truncated, err
}
