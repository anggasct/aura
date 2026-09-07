package runtimeengine

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/runtime/channelhost"
)

type Host struct {
	runtime  *Engine
	adapters []runtimechannelhost.ChannelPort
	logger   *slog.Logger

	mu        sync.Mutex
	runCancel context.CancelFunc
	running   bool
	wg        sync.WaitGroup
}

func NewHost(runtime *Engine, adapters []runtimechannelhost.ChannelPort, logger *slog.Logger) (*Host, error) {
	if runtime == nil {
		return nil, invalidArgument("runtime must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Host{runtime: runtime, adapters: adapters, logger: logger}, nil
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
