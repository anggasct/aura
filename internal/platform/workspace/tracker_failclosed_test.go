package workspace

import (
	"context"
	"errors"
	"testing"
)

type failingApplyStore struct {
	applyErr    error
	wideErr     error
	wideCalls   int
	wideSource  string
	wideSession string
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
func (f *failingApplyStore) HasRecords(context.Context) (bool, error)         { return false, nil }
func (f *failingApplyStore) HasWorkspaceWide(context.Context) (bool, error)   { return false, nil }
func (f *failingApplyStore) OldestSource(context.Context) (string, bool, error) {
	return "", false, nil
}
func (f *failingApplyStore) MatchesFile(context.Context, string, string, int64, int64) (bool, error) {
	return false, nil
}
func (f *failingApplyStore) MatchesEntry(context.Context, string, int64, int64, bool) (bool, error) {
	return false, nil
}
func (f *failingApplyStore) ApplyWalkResults(context.Context, WalkResult, string, string) (ApplyWalkStats, error) {
	return ApplyWalkStats{}, f.applyErr
}

func TestEndEffectFailsClosedOnApplyWalkResultsError(t *testing.T) {
	t.Run("fallback workspace-wide record is written and output stays tainted", func(t *testing.T) {
		applyErr := errors.New("sqlite busy")
		backend := &failingApplyStore{applyErr: applyErr}
		var metrics []string
		tracker := &Tracker{
			root:       t.TempDir(),
			backend:    backend,
			maxEntries: DefaultScanEntries,
			onRecord:   func(_ context.Context, kind string) { metrics = append(metrics, kind) },
		}
		scope, err := tracker.BeginEffect(t.Context(), true, "web_fetch", "sess-fail-1")
		if err != nil {
			t.Fatalf("BeginEffect: %v", err)
		}
		tainted, endErr := tracker.EndEffect(t.Context(), &scope)
		if !tainted {
			t.Fatal("EndEffect tainted = false, want true (fail-closed)")
		}
		if !errors.Is(endErr, applyErr) {
			t.Fatalf("EndEffect err = %v, want %v", endErr, applyErr)
		}
		if backend.wideCalls != 1 || backend.wideSource != "web_fetch" || backend.wideSession != "sess-fail-1" {
			t.Fatalf("fallback wide calls = %d (%q, %q), want 1 (web_fetch, sess-fail-1)", backend.wideCalls, backend.wideSource, backend.wideSession)
		}
		if len(metrics) != 1 || metrics[0] != RecordKindWide {
			t.Fatalf("metrics = %v, want [%s]", metrics, RecordKindWide)
		}
	})

	t.Run("double failure still fails closed with joined error", func(t *testing.T) {
		applyErr := errors.New("apply failed")
		wideErr := errors.New("wide fallback failed")
		backend := &failingApplyStore{applyErr: applyErr, wideErr: wideErr}
		tracker := &Tracker{
			root:       t.TempDir(),
			backend:    backend,
			maxEntries: DefaultScanEntries,
		}
		scope, err := tracker.BeginEffect(t.Context(), true, "exec", "sess-fail-2")
		if err != nil {
			t.Fatalf("BeginEffect: %v", err)
		}
		tainted, endErr := tracker.EndEffect(t.Context(), &scope)
		if !tainted {
			t.Fatal("EndEffect tainted = false, want true (fail-closed)")
		}
		if !errors.Is(endErr, applyErr) || !errors.Is(endErr, wideErr) {
			t.Fatalf("EndEffect err = %v, want both apply and wide errors", endErr)
		}
	})
}
