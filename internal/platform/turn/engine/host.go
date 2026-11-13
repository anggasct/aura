package runtimeengine

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/platform/turn/channelhost"
)

type Host struct {
	runtime *Engine
	logger  *slog.Logger

	mu        sync.Mutex
	runCancel context.CancelFunc
	running   bool
	wg        sync.WaitGroup
}

func NewHost(engine *Engine, logger *slog.Logger) (*Host, error) {
	if engine == nil {
		return nil, invalidArgument("engine must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Host{runtime: engine, logger: logger}, nil
}

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

	for _, adapter := range h.runtime.adapters {
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
	return nil
}
