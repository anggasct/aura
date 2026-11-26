package health

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

type ProbeServerOptions struct {
	Listen        string
	Version       string
	Registry      *Registry
	Evaluate      func(ctx context.Context) []Finding
	CheckTimeout  time.Duration
	CheckInterval time.Duration
	Tracker       *StateTracker
}

type ProbeServer struct {
	listen    string
	handler   http.Handler
	readiness *Readiness
	evaluate  func(ctx context.Context) []Finding
	tracker   *StateTracker
	interval  time.Duration
}

func NewProbeServer(opts *ProbeServerOptions) (*ProbeServer, error) {
	if opts == nil || (opts.Registry == nil && opts.Evaluate == nil) {
		return nil, errors.New("probe server requires a health registry")
	}
	readiness := NewReadiness()
	evaluate := opts.Evaluate
	if evaluate == nil {
		budget := opts.CheckTimeout
		evaluate = func(ctx context.Context) []Finding {
			if budget > 0 {
				evalCtx, cancel := context.WithTimeout(ctx, budget)
				defer cancel()
				return opts.Registry.Evaluate(evalCtx)
			}
			return opts.Registry.Evaluate(ctx)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", LivenessHandler(NewLiveness(), opts.Version, nil))
	mux.HandleFunc("/readyz", ReadinessHandler(readiness, evaluate, opts.Version, nil))
	return &ProbeServer{
		listen:    opts.Listen,
		handler:   mux,
		readiness: readiness,
		evaluate:  evaluate,
		tracker:   opts.Tracker,
		interval:  opts.CheckInterval,
	}, nil
}

func (p *ProbeServer) Name() string { return "health-probes" }

func (p *ProbeServer) Draining() bool { return p.readiness.Draining() }

func (p *ProbeServer) StartObserver(ctx context.Context) <-chan struct{} {
	return p.startObserver(ctx)
}

func (p *ProbeServer) Start(ctx context.Context) error {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", p.listen)
	if err != nil {
		return fmt.Errorf("probe listener: bind %s: %w", p.listen, err)
	}
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

func (p *ProbeServer) observeLoop(ctx context.Context) func() {
	observerCtx, cancel := context.WithCancel(ctx)
	stopped := p.startObserver(observerCtx)
	return func() {
		cancel()
		<-stopped
	}
}

func (p *ProbeServer) startObserver(ctx context.Context) <-chan struct{} {
	stopped := make(chan struct{})
	if p.tracker == nil || p.interval <= 0 {
		close(stopped)
		return stopped
	}
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				evalCtx, evalCancel := context.WithTimeout(context.WithoutCancel(ctx), p.interval)
				findings := p.evaluate(evalCtx)
				evalCancel()
				persistBudget := p.interval
				if persistBudget < 5*time.Second {
					persistBudget = 5 * time.Second
				}
				persistCtx, persistCancel := context.WithTimeout(ctx, persistBudget)
				_, err := p.tracker.Observe(persistCtx, findings)
				persistCancel()
				if err != nil {
					continue
				}
			}
		}
	}()
	return stopped
}
