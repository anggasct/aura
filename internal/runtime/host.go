package runtime

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/runtime/channelhost"
)

// Host owns the runtime and its channel adapters. It hands each adapter the
// runtime as its ingress sink and coordinates a single ordered shutdown: stop
// ingress, let the adapters return, then drain the runtime so every accepted
// turn reaches a durable terminal before the adapters close.
type Host struct {
	runtime  *Engine
	adapters []runtimechannelhost.ChannelPort
	logger   *slog.Logger

	mu        sync.Mutex
	runCancel context.CancelFunc
	running   bool
	wg        sync.WaitGroup
}

// NewHost builds a host over the runtime and its adapters. Adapters may be
// empty; the runtime must not be nil.
func NewHost(runtime *Engine, adapters []runtimechannelhost.ChannelPort, logger *slog.Logger) (*Host, error) {
	if runtime == nil {
		return nil, invalidArgument("runtime must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Host{runtime: runtime, adapters: adapters, logger: logger}, nil
}

// Start launches every adapter, handing each the runtime as its ingress sink.
// Adapter goroutines run until Shutdown cancels their context; an adapter
// that returns with an error outside shutdown is logged, not fatal.
func (h *Host) Start(ctx context.Context) error {
	h.mu.Lock()
	if h.running {
		h.mu.Unlock()
		return invalidArgument("host already started")
	}
	runCtx, cancel := context.WithCancel(ctx)
	h.runCancel = cancel
	h.running = true
	h.mu.Unlock()

	for _, adapter := range h.adapters {
		h.wg.Add(1)
		go func(a runtimechannelhost.ChannelPort) {
			defer h.wg.Done()
			if err := a.Start(runCtx, h.runtime); err != nil && runCtx.Err() == nil {
				h.logger.ErrorContext(runCtx, "channel adapter stopped with error", "error", err)
			}
		}(adapter)
	}
	return nil
}

// Shutdown stops ingress by cancelling the adapter contexts, waits for the
// adapters to return within the runtime's shutdown grace, then drains the
// runtime. An adapter that ignores cancellation is abandoned after the grace
// period; the runtime drain still bounds total shutdown time.
func (h *Host) Shutdown(ctx context.Context) error {
	h.mu.Lock()
	cancel := h.runCancel
	h.running = false
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}

	adaptersDone := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(adaptersDone)
	}()

	grace := h.runtime.cfg.ShutdownTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < grace {
			grace = remaining
		}
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-adaptersDone:
	case <-timer.C:
		h.logger.WarnContext(ctx, "channel adapters did not stop within grace; abandoning")
	case <-ctx.Done():
	}
	return h.runtime.Shutdown(ctx)
}
