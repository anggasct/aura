package server

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"
)

type Lifecycle interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

type LifecycleOptions struct {
	StopTimeout time.Duration
	Logger      *slog.Logger
}

type lifecycleEntry struct {
	name    string
	start   func(ctx context.Context) error
	stop    func(ctx context.Context) error
	running bool
}

type LifecycleGroup struct {
	mu          sync.Mutex
	entries     []*lifecycleEntry
	stopTimeout time.Duration
	logger      *slog.Logger
	started     bool
	stopped     bool
}

func NewLifecycleGroup(opts LifecycleOptions) *LifecycleGroup {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	timeout := opts.StopTimeout
	if timeout <= 0 {
		timeout = defaultShutdownTimeout
	}
	return &LifecycleGroup{stopTimeout: timeout, logger: logger}
}

func (g *LifecycleGroup) Add(l Lifecycle) error {
	if l == nil {
		return &Error{Code: ErrorCodeInvalidArgument, Detail: "lifecycle must not be nil"}
	}
	return g.register(&lifecycleEntry{name: l.Name(), start: l.Start, stop: l.Stop})
}

func (g *LifecycleGroup) Defer(name string, stop func(ctx context.Context) error) error {
	if stop == nil {
		return &Error{Code: ErrorCodeInvalidArgument, Detail: "deferred stop must not be nil"}
	}
	return g.register(&lifecycleEntry{name: name, stop: stop, running: true})
}

func (g *LifecycleGroup) register(entry *lifecycleEntry) error {
	if entry.name == "" {
		return &Error{Code: ErrorCodeInvalidArgument, Detail: "lifecycle name must not be empty"}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.started || g.stopped {
		return &Error{Code: ErrorCodeInvalidArgument, Detail: "lifecycle group no longer accepts entries"}
	}
	for _, registered := range g.entries {
		if registered.name == entry.name {
			return &Error{Code: ErrorCodeInvalidArgument, Detail: "lifecycle " + entry.name + " is already registered"}
		}
	}
	g.entries = append(g.entries, entry)
	return nil
}

func (g *LifecycleGroup) Names() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	names := make([]string, len(g.entries))
	for i, entry := range g.entries {
		names[i] = entry.name
	}
	return names
}

func (g *LifecycleGroup) Start(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.started || g.stopped {
		return &Error{Code: ErrorCodeInvalidArgument, Detail: "lifecycle group was already started or stopped"}
	}
	g.started = true
	for _, entry := range g.entries {
		if entry.start == nil {
			continue
		}
		if err := entry.start(ctx); err != nil {
			return err
		}
		entry.running = true
		g.logger.DebugContext(ctx, "lifecycle started", "component", "server", "name", entry.name)
	}
	return nil
}

func (g *LifecycleGroup) Stop(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped {
		return nil
	}
	g.stopped = true
	var failures []error
	for _, entry := range slices.Backward(g.entries) {
		if !entry.running {
			continue
		}
		entry.running = false
		if err := g.stopEntry(ctx, entry); err != nil {
			failures = append(failures, err)
			continue
		}
		g.logger.DebugContext(ctx, "lifecycle stopped", "component", "server", "name", entry.name)
	}
	return errors.Join(failures...)
}

func (g *LifecycleGroup) stopEntry(ctx context.Context, entry *lifecycleEntry) error {
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), g.stopTimeout)
	defer cancel()
	return entry.stop(stopCtx)
}

type background struct {
	name string
	run  func(ctx context.Context)

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func Background(name string, run func(ctx context.Context)) Lifecycle {
	return &background{name: name, run: run}
}

func (b *background) Name() string { return b.name }

func (b *background) Start(ctx context.Context) error {
	if b.run == nil {
		return &Error{Code: ErrorCodeInvalidArgument, Detail: "background " + b.name + " has no run function"}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done != nil {
		return &Error{Code: ErrorCodeInvalidArgument, Detail: "background " + b.name + " was already started"}
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	b.cancel, b.done = cancel, done
	go func() {
		defer close(done)
		b.run(runCtx)
	}()
	return nil
}

func (b *background) Stop(ctx context.Context) error {
	b.mu.Lock()
	cancel, done := b.cancel, b.done
	b.mu.Unlock()
	if done == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return &Error{Code: ErrorCodeStopTimeout, Detail: b.name + " did not stop"}
	}
}
