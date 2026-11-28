package workspace

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"
)

const (
	RecordKindWrite   = "write"
	RecordKindExec    = "exec"
	RecordKindCarried = "carried"
	RecordKindWide    = "wide"
)

type DirEntryStat struct {
	Path    string
	Device  int64
	Inode   int64
	HasStat bool
}

type EffectScope struct {
	active     bool
	hadRecords bool
	start      time.Time
	source     string
	sessionID  string
	kind       string
}

type taintStoreBackend interface {
	RecordFile(ctx context.Context, path, sha256Hex string, device, inode int64, source, sessionID string) error
	RecordWorkspaceWide(ctx context.Context, source, sessionID string) (bool, error)
	ClearPath(ctx context.Context, path string) (int64, error)
	HasRecords(ctx context.Context) (bool, error)
	HasWorkspaceWide(ctx context.Context) (bool, error)
	OldestSource(ctx context.Context) (string, bool, error)
	MatchesFile(ctx context.Context, path, sha256Hex string, device, inode int64) (bool, error)
	MatchesEntry(ctx context.Context, path string, device, inode int64, hasStat bool) (bool, error)
	ApplyWalkResults(ctx context.Context, res WalkResult, source, sessionID string) (ApplyWalkStats, error)
}

type Tracker struct {
	root       string
	store      *TaintStore
	backend    taintStoreBackend
	maxEntries int
	onRecord   func(ctx context.Context, kind string)
	walkCount  atomic.Int64
}

func NewTracker(root string, store *TaintStore, maxEntries int, onRecord func(ctx context.Context, kind string)) *Tracker {
	if maxEntries <= 0 {
		maxEntries = DefaultScanEntries
	}
	var backend taintStoreBackend
	if store != nil {
		backend = store
	}
	return &Tracker{
		root:       root,
		store:      store,
		backend:    backend,
		maxEntries: maxEntries,
		onRecord:   onRecord,
	}
}

func (t *Tracker) Store() *TaintStore {
	if t == nil {
		return nil
	}
	return t.store
}

func (t *Tracker) WalkCount() int64 {
	if t == nil {
		return 0
	}
	return t.walkCount.Load()
}

func (t *Tracker) recordMetric(ctx context.Context, kind string) {
	if t != nil && t.onRecord != nil {
		t.onRecord(ctx, kind)
	}
}

func (t *Tracker) OnWriteFile(ctx context.Context, relative string, tainted bool, taintSource, sessionID string, overwrite bool) error {
	if t == nil || t.backend == nil {
		return nil
	}
	if !tainted {
		if overwrite {
			_, err := t.backend.ClearPath(ctx, relative)
			return err
		}
		return nil
	}
	sha256Hex, dev, ino, err := statAndHashWorkspaceFile(t.root, relative)
	if err != nil {
		return err
	}
	if err := t.backend.RecordFile(ctx, relative, sha256Hex, dev, ino, taintSource, sessionID); err != nil {
		return err
	}
	t.recordMetric(ctx, RecordKindWrite)
	return nil
}

func (t *Tracker) CheckReadFile(ctx context.Context, relative, sha256Hex string, device, inode int64) (bool, error) {
	if t == nil || t.backend == nil {
		return false, nil
	}
	return t.backend.MatchesFile(ctx, relative, sha256Hex, device, inode)
}

func (t *Tracker) CheckListDir(ctx context.Context, entries []DirEntryStat) (bool, error) {
	if t == nil || t.backend == nil {
		return false, nil
	}
	has, err := t.backend.HasRecords(ctx)
	if err != nil {
		return true, err
	}
	if !has {
		return false, nil
	}
	wide, err := t.backend.HasWorkspaceWide(ctx)
	if err != nil {
		return true, err
	}
	if wide {
		return true, nil
	}
	for i := range entries {
		matched, err := t.backend.MatchesEntry(ctx, entries[i].Path, entries[i].Device, entries[i].Inode, entries[i].HasStat)
		if err != nil {
			return true, err
		}
		if matched {
			return true, nil
		}
	}
	return false, nil
}

func (t *Tracker) BeginEffect(ctx context.Context, tainted bool, taintSource, sessionID string) (EffectScope, error) {
	if t == nil || t.backend == nil {
		return EffectScope{}, nil
	}
	hadRecords, err := t.backend.HasRecords(ctx)
	if err != nil {
		return EffectScope{
			active:     true,
			hadRecords: true,
			start:      coarseNow(),
			source:     boundSource(taintSource),
			sessionID:  sessionID,
			kind:       RecordKindExec,
		}, err
	}
	if !tainted && !hadRecords {
		return EffectScope{}, nil
	}
	source := taintSource
	kind := RecordKindExec
	if !tainted {
		kind = RecordKindCarried
		if oldest, found, oerr := t.backend.OldestSource(ctx); oerr == nil && found {
			source = oldest
		}
	}
	return EffectScope{
		active:     true,
		hadRecords: hadRecords,
		start:      coarseNow(),
		source:     boundSource(source),
		sessionID:  sessionID,
		kind:       kind,
	}, nil
}

func (t *Tracker) EndEffect(ctx context.Context, scope *EffectScope) (bool, error) {
	if t == nil || t.backend == nil || scope == nil || !scope.active {
		return false, nil
	}
	t.walkCount.Add(1)
	walkRes, err := walkWorkspace(ctx, t.root, scope.start, t.maxEntries)
	if err != nil {
		walkRes = WalkResult{ExceededBound: true}
	}
	stats, applyErr := t.backend.ApplyWalkResults(ctx, walkRes, scope.source, scope.sessionID)
	if applyErr != nil {
		slog.WarnContext(ctx, "workspace taint walk apply failed; attempting workspace-wide fallback",
			"component", "workspace",
			"error", applyErr,
		)
		created, wideErr := t.backend.RecordWorkspaceWide(ctx, scope.source, scope.sessionID)
		if wideErr == nil && created {
			t.recordMetric(ctx, RecordKindWide)
		}
		return true, errors.Join(applyErr, wideErr)
	}
	if stats.Wide > 0 {
		t.recordMetric(ctx, RecordKindWide)
	}
	for range stats.Recorded {
		t.recordMetric(ctx, scope.kind)
	}
	hasAfter, hasErr := t.backend.HasRecords(ctx)
	if hasErr != nil {
		return true, hasErr
	}
	return scope.hadRecords || hasAfter, err
}
