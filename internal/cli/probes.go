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
	"github.com/anggasct/aura/internal/store"
)

// probeListener serves the loopback /livez and /readyz endpoints inside the
// server lifecycle. It owns a health registry so findings are recomputed per
// probe request, and a transition tracker whose observations persist as
// runtime events; liveness is process-local and never consults either.
type probeListener struct {
	listen    string
	handler   http.Handler
	readiness *health.Readiness
	evaluate  func(ctx context.Context) []health.Finding
	tracker   *health.StateTracker
	interval  time.Duration
}

func buildProbeListener(cfg *config.Config, capabilities []health.CapabilityStatus, events store.EventStore, sessions store.SessionService) (*probeListener, error) {
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
	var tracker *health.StateTracker
	if events != nil && sessions != nil {
		log := newHealthEventLog(events, sessions)
		// A candidate state must hold through one evaluation cycle before it
		// commits, and committed transitions stay at least two cycles apart:
		// flap resistance derived from the configured cadence.
		policy := health.TransitionPolicy{
			StableFor: time.Duration(cfg.Health.CheckInterval),
			Cooldown:  max(2*time.Duration(cfg.Health.CheckInterval), time.Minute),
		}
		tracker, err = health.NewStateTracker(policy, log.sink, log.history)
		if err != nil {
			return nil, err
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", health.LivenessHandler(health.NewLiveness(), version, nil))
	mux.HandleFunc("/readyz", health.ReadinessHandler(readiness, evaluate, version, nil))
	return &probeListener{
		listen:    cfg.Health.Listen,
		handler:   mux,
		readiness: readiness,
		evaluate:  evaluate,
		tracker:   tracker,
		interval:  time.Duration(cfg.Health.CheckInterval),
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
	stopObserving := p.observeLoop(ctx)

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
		stopObserving()
		return fmt.Errorf("probe listener: %w", err)
	case <-ctx.Done():
		// Shutdown ordering: the drain flag flips synchronously before
		// this or any other listener tears its socket down, so no ingress
		// is rejected while readiness still claims open. Liveness is
		// never cleared — it stays alive through graceful drain.
		p.readiness.SetDraining(true)
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			stopObserving()
			return fmt.Errorf("probe listener shutdown: %w", err)
		}
		stopObserving()
		return nil
	}
}

// observeLoop folds periodic evaluations into the transition tracker until
// the context is cancelled. It exists only when a durable log is wired.
func (p *probeListener) observeLoop(ctx context.Context) func() {
	if p.tracker == nil || p.interval <= 0 {
		return func() {}
	}
	loopCtx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
				evalCtx, evalCancel := context.WithTimeout(loopCtx, p.interval)
				findings := p.evaluate(evalCtx)
				evalCancel()
				// Persistence runs on its own shutdown-bounded context: the
				// evaluation timeout must not cancel the durable append.
				persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(loopCtx), p.interval)
				_, err := p.tracker.Observe(persistCtx, findings)
				persistCancel()
				if err != nil {
					// Persistence retried on the next tick; state does not
					// advance past an unwritten transition.
					continue
				}
			}
		}
	}()
	return cancel
}
