package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
)

const (
	defaultShutdownTimeout = 30 * time.Second
	forcedExitCode         = 130
)

type Listener interface {
	Name() string
	Start(ctx context.Context) error
}

type Options struct {
	ShutdownTimeout time.Duration
	Logger          *slog.Logger
}

type Server struct {
	listeners       []Listener
	shutdownTimeout time.Duration
	logger          *slog.Logger
	exitFunc        func(code int)
}

func New(opts Options) *Server {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	timeout := opts.ShutdownTimeout
	if timeout <= 0 {
		timeout = defaultShutdownTimeout
	}
	return &Server{
		shutdownTimeout: timeout,
		logger:          logger,
		exitFunc:        os.Exit,
	}
}

func (s *Server) Add(l Listener) {
	s.listeners = append(s.listeners, l)
}

// Run starts all registered listeners and blocks until a shutdown signal or a
// listener failure. The first signal drains listeners within the shutdown
// timeout; a second signal forces an immediate exit. It returns the first
// listener error, or nil on a clean shutdown.
func (s *Server) Run(ctx context.Context) error {
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	g, gctx := errgroup.WithContext(runCtx)
	for _, l := range s.listeners {
		g.Go(func() error { return s.runListener(gctx, l) })
	}

	waitResult := make(chan error, 1)
	go func() { waitResult <- g.Wait() }()

	var firstErr error
	signalled := false
	if len(s.listeners) == 0 {
		sig := <-sigCh
		s.logger.Info("received shutdown signal", "component", "server", "signal", sig.String())
		signalled = true
	} else {
		select {
		case sig := <-sigCh:
			s.logger.Info("received shutdown signal", "component", "server", "signal", sig.String())
			signalled = true
		case firstErr = <-waitResult:
		}
	}

	cancel()
	if signalled {
		go s.forceExitOnSecondSignal(sigCh)
	}

	if firstErr == nil {
		select {
		case firstErr = <-waitResult:
		case <-time.After(s.shutdownTimeout):
			s.logger.Warn("shutdown timed out; forcing exit", "component", "server", "timeout", s.shutdownTimeout)
		}
	}

	if firstErr != nil {
		s.logger.Error("server stopped with error", "component", "server", "error", firstErr)
		return firstErr
	}
	s.logger.Info("server stopped", "component", "server")
	return nil
}

func (s *Server) runListener(ctx context.Context, l Listener) (err error) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("listener panic",
				"component", "server",
				"listener", l.Name(),
				"panic", fmt.Sprint(r),
				"stack", string(debug.Stack()),
			)
			err = fmt.Errorf("listener %s panicked: %v", l.Name(), r)
		}
	}()
	s.logger.Info("starting listener", "component", "server", "listener", l.Name())
	if err := l.Start(ctx); err != nil {
		s.logger.Error("listener failed", "component", "server", "listener", l.Name(), "error", err)
		return err
	}
	return nil
}

func (s *Server) forceExitOnSecondSignal(sigCh <-chan os.Signal) {
	sig := <-sigCh
	s.logger.Warn("forced shutdown", "component", "server", "signal", sig.String())
	s.exitFunc(forcedExitCode)
}
