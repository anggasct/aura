package workspace

import (
	"context"
	"errors"
	"log/slog"
	"testing"
)

type failingApplyStore struct {
	applyErr      error
	wideErr       error
	hasRecordsErr error
	wideCalls     int
	wideSource    string
	wideSession   string
}

func (f *failingApplyStore) RecordFile(context.Context, string, string, int64, int64, string, string) error {
	return nil
}

func (f *failingApplyStore) RecordWorkspaceWide(_ context.Context, source, sessionID string) (bool, error) {
	f.wideCalls++
	f.wideSource = source
	f.wideSession = sessionID
	if f.wideErr != nil {
		return false, f.wideErr
	}
	return true, nil
}

func (f *failingApplyStore) ClearPath(context.Context, string) (int64, error) { return 0, nil }
func (f *failingApplyStore) HasRecords(context.Context) (bool, error) {
	return false, f.hasRecordsErr
}
func (f *failingApplyStore) HasWorkspaceWide(context.Context) (bool, error) { return false, nil }
func (f *failingApplyStore) OldestSource(context.Context) (source string, found bool, err error) {
	return "", false, nil
}
func (f *failingApplyStore) MatchesFile(context.Context, string, string, int64, int64) (bool, error) {
	return false, nil
}
func (f *failingApplyStore) MatchesEntry(context.Context, string, int64, int64, bool) (bool, error) {
	return false, nil
}
func (f *failingApplyStore) ApplyWalkResults(context.Context, WalkResult, string, string) (WalkStats, error) {
	return WalkStats{}, f.applyErr
}

func TestEndEffectFailsClosedOnApplyWalkResultsError(t *testing.T) {
	t.Run("fallback workspace-wide record is written and output stays tainted", func(t *testing.T) {
		backend := &failingApplyStore{applyErr: errors.New("sqlite busy")}
		var metrics []string
		tracker := &Tracker{
			root:       t.TempDir(),
			backend:    backend,
			maxEntries: DefaultScanEntries,
			onRecord:   func(_ context.Context, kind string) { metrics = append(metrics, kind) },
			logger:     slog.New(slog.DiscardHandler),
		}
		scope := tracker.BeginEffect(t.Context(), true, "web_fetch", "sess-fail-1")
		tainted := tracker.EndEffect(t.Context(), &scope)
		if !tainted {
			t.Fatal("EndEffect tainted = false, want true (fail-closed)")
		}
		if backend.wideCalls != 1 || backend.wideSource != "web_fetch" || backend.wideSession != "sess-fail-1" {
			t.Fatalf("fallback wide calls = %d (%q, %q), want 1 (web_fetch, sess-fail-1)", backend.wideCalls, backend.wideSource, backend.wideSession)
		}
		if len(metrics) != 1 || metrics[0] != RecordKindWide {
			t.Fatalf("metrics = %v, want [%s]", metrics, RecordKindWide)
		}
	})

	t.Run("fallback failure still fails closed with the wide record attempted", func(t *testing.T) {
		backend := &failingApplyStore{applyErr: errors.New("apply failed"), wideErr: errors.New("wide fallback failed")}
		tracker := &Tracker{
			root:       t.TempDir(),
			backend:    backend,
			maxEntries: DefaultScanEntries,
			logger:     slog.New(slog.DiscardHandler),
		}
		scope := tracker.BeginEffect(t.Context(), true, "exec", "sess-fail-2")
		if !tracker.EndEffect(t.Context(), &scope) {
			t.Fatal("EndEffect tainted = false, want true (fail-closed)")
		}
		if backend.wideCalls != 1 {
			t.Fatalf("wide fallback calls = %d, want 1", backend.wideCalls)
		}
	})
}

func TestBeginEffectFailsClosedOnHasRecordsError(t *testing.T) {
	backend := &failingApplyStore{hasRecordsErr: errors.New("has records failed")}
	tracker := &Tracker{
		root:       t.TempDir(),
		backend:    backend,
		maxEntries: DefaultScanEntries,
		logger:     slog.New(slog.DiscardHandler),
	}
	scope := tracker.BeginEffect(t.Context(), false, "", "sess-fail-3")
	if !scope.active || !scope.hadRecords {
		t.Fatalf("BeginEffect scope = %+v, want an active fail-closed scope", scope)
	}
}
