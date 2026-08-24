package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/sandbox"
)

// probeListener serves the loopback /livez and /readyz endpoints inside the
// server lifecycle. It owns a health registry so findings are recomputed per
// probe request; liveness is process-local and never consults it.
type probeListener struct {
	listen      string
	checkBudget time.Duration
	handler     http.Handler
	readiness   *health.Readiness
}

func buildProbeListener(cfg *config.Config, capabilities []health.CapabilityStatus) (*probeListener, error) {
	if cfg == nil {
		return nil, errors.New("probe listener requires configuration")
	}
	registry, err := buildHealthRegistry(cfg, capabilities, sandbox.Negotiate, processProbe)
	if err != nil {
		return nil, err
	}
	readiness := health.NewReadiness()
	budget := time.Duration(cfg.Health.CheckTimeout)
	evaluate := func(ctx context.Context) []health.Finding {
		evalCtx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		return registry.Evaluate(evalCtx)
	}
	mux := http.NewServeMux()
	mux.Handle("/livez", health.LivenessHandler(health.NewLiveness(), version, nil))
	mux.Handle("/readyz", health.ReadinessHandler(readiness, evaluate, version, nil))
	return &probeListener{
		listen:      cfg.Health.Listen,
		checkBudget: budget,
		handler:     mux,
		readiness:   readiness,
	}, nil
}

func (p *probeListener) Name() string { return "health-probes" }

func (p *probeListener) Start(ctx context.Context) error {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", p.listen)
	if err != nil {
		return fmt.Errorf("probe listener: bind %s: %w", p.listen, err)
	}
	// Readiness turns true only once the endpoint is actually serving.
	p.readiness.SetStarted()
	server := &http.Server{
		Handler:           p.handler,
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("probe listener: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("probe listener shutdown: %w", err)
		}
		return nil
	}
}
