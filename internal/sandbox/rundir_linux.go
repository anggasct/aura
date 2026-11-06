//go:build linux

package sandbox

import (
	"context"
	"io/fs"
	"os"
	"syscall"
	"time"
)

const (
	runDirSampleInterval = 250 * time.Millisecond
	runDirWalkBudget     = 200 * time.Millisecond
	runDirMaxEntries     = 10000
	statBlockBytes       = 512
)

type runDirBounds struct {
	walk    time.Duration
	entries int
}

func defaultRunDirBounds() runDirBounds {
	return runDirBounds{walk: runDirWalkBudget, entries: runDirMaxEntries}
}

type runDirUsage struct {
	bytes   int64
	largest int64
	entries int
	capped  bool
}

func (u runDirUsage) exceeds(limit int64) bool {
	return u.capped || u.bytes > limit
}

func measureRunDir(ctx context.Context, dir string, bounds runDirBounds) runDirUsage {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return runDirUsage{}
	}
	defer func() { _ = root.Close() }()
	walkCtx, cancel := context.WithTimeout(ctx, bounds.walk)
	defer cancel()
	var usage runDirUsage
	_ = fs.WalkDir(root.FS(), ".", func(_ string, entry fs.DirEntry, err error) error {
		if walkCtx.Err() != nil {
			return fs.SkipAll
		}
		if err != nil {
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if usage.entries >= bounds.entries {
			usage.capped = true
			return fs.SkipAll
		}
		usage.entries++
		if info, infoErr := entry.Info(); infoErr == nil {
			usage.bytes += allocatedBytes(info)
			if info.Mode().IsRegular() {
				usage.largest = max(usage.largest, info.Size())
			}
		}
		return nil
	})
	if ctx.Err() == nil && walkCtx.Err() != nil {
		usage.capped = true
	}
	return usage
}

func allocatedBytes(info fs.FileInfo) int64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Blocks * statBlockBytes
	}
	return info.Size()
}

type runDirWatcher struct {
	dir      string
	limit    int64
	exceeded chan struct{}
	stopped  chan struct{}
	cancel   context.CancelFunc
}

func startRunDirWatcher(ctx context.Context, dir string, limit int64) *runDirWatcher {
	watchCtx, cancel := context.WithCancel(ctx)
	watcher := &runDirWatcher{dir: dir, limit: limit, exceeded: make(chan struct{}), stopped: make(chan struct{}), cancel: cancel}
	go watcher.run(watchCtx)
	return watcher
}

func (w *runDirWatcher) run(ctx context.Context) {
	defer close(w.stopped)
	ticker := time.NewTicker(runDirSampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if measureRunDir(ctx, w.dir, defaultRunDirBounds()).exceeds(w.limit) && ctx.Err() == nil {
				close(w.exceeded)
				return
			}
		}
	}
}

func (w *runDirWatcher) stop() {
	w.cancel()
	<-w.stopped
}
